package live

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/tomasz-tomczyk/crit/internal/browser"
	"github.com/tomasz-tomczyk/crit/internal/clicmd"
	"github.com/tomasz-tomczyk/crit/internal/config"
	"github.com/tomasz-tomczyk/crit/internal/daemon"
	"github.com/tomasz-tomczyk/crit/internal/focus"
)

type smokeKind int

const (
	smokeOK          smokeKind = iota
	smokeConnRefused           // connection refused / DNS failure
	smokeNon2xx                // non-2xx HTTP status (warn, continue)
	smokeNonHTML               // non-text/html content type (fatal)
	smokeMissingBody           // HTML without </body> (warn, continue)
)

type smokeResult struct {
	kind                 smokeKind
	fatal                bool
	message              string
	hasCSPFrameAncestors bool
	frameworkNotes       []string
}

// phoenixRE matches one of three discriminating Phoenix LiveView markers.
// A bare "phx-" substring is too loose — third-party CSS libraries use
// `phx-` prefixes in unrelated contexts.
var phoenixRE = regexp.MustCompile(`phx-track-static|phx-hook=|phx-main`)

// detectFrameworks returns informational notes (one per detected framework)
// produced by probing the upstream HTML body. None block startup.
func detectFrameworks(body []byte) []string {
	var notes []string
	if phoenixRE.Match(body) {
		notes = append(notes, "Phoenix LiveView detected. Make sure your dev endpoint allows iframing — strip CSP locally if needed.")
	}
	if bytes.Contains(body, []byte(`/@vite/client`)) {
		notes = append(notes, "Vite dev server detected. WebSocket HMR will be proxied automatically.")
	}
	if bytes.Contains(body, []byte(`id="__next"`)) {
		notes = append(notes, "Next.js dev detected. SPA route changes via `pushState` are supported.")
	}
	return notes
}

var smokeClient = &http.Client{Timeout: 10 * time.Second}

var (
	startLiveDaemon                                             = daemon.StartDaemon
	runLiveClient                                               = daemon.RunReviewClient
	installLiveDaemonSignalHandler                              = installDaemonSignalHandler
	openLiveBrowser                                             = browser.OpenBrowserWithCommand
	lookupPortlessServiceURL       func(string) (string, error) = defaultLookupPortlessServiceURL
	launchLiveBrowser                                           = func(url, openCmd string) {
		go openLiveBrowser(url, openCmd)
	}
)

func runSmokeTest(origin, cookies string) smokeResult {
	req, err := http.NewRequest(http.MethodGet, origin, nil)
	if err != nil {
		return smokeResult{
			kind:    smokeConnRefused,
			fatal:   true,
			message: fmt.Sprintf("is your dev server running at %s? (%v)", origin, err),
		}
	}
	if cookies != "" {
		req.Header.Set("Cookie", cookies)
	}
	resp, err := smokeClient.Do(req)
	if err != nil {
		return smokeResult{
			kind:    smokeConnRefused,
			fatal:   true,
			message: fmt.Sprintf("is your dev server running at %s? (%v)", origin, err),
		}
	}
	defer resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	hasCSP := strings.Contains(strings.ToLower(csp), "frame-ancestors")

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return smokeResult{
			kind:                 smokeNon2xx,
			fatal:                false,
			message:              fmt.Sprintf("upstream returned %d; live mode may not work as expected", resp.StatusCode),
			hasCSPFrameAncestors: hasCSP,
		}
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		return smokeResult{
			kind:    smokeNonHTML,
			fatal:   true,
			message: fmt.Sprintf("live mode needs HTML; this URL returns %q. Did you mean a different URL?", ct),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return smokeResult{kind: smokeOK, hasCSPFrameAncestors: hasCSP}
	}

	notes := detectFrameworks(body)
	if !strings.Contains(strings.ToLower(string(body)), "</body>") {
		return smokeResult{
			kind:                 smokeMissingBody,
			fatal:                false,
			message:              "couldn't find a </body> injection target; live-mode agent may not boot",
			hasCSPFrameAncestors: hasCSP,
			frameworkNotes:       notes,
		}
	}

	return smokeResult{kind: smokeOK, hasCSPFrameAncestors: hasCSP, frameworkNotes: notes}
}

// LooksLikeLiveArgs returns true when args is a single http/https URL.
// GitHub PR URLs are excluded — those route to range mode via --pr.
func LooksLikeLiveArgs(args []string) bool {
	if len(args) != 1 {
		return false
	}
	if focus.LooksLikePRURL(args[0]) {
		return false
	}
	u, err := url.Parse(args[0])
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func connectToLiveDaemon(key string, noOpen bool, openCmd string, quiet bool) bool {
	entry, alive := daemon.FindAliveSession(key)
	if !alive {
		return false
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "[crit] connected to live daemon at %s (proxy :%d)\n",
			entry.BaseURL(), entry.Port+1)
		fmt.Fprintf(os.Stderr, "[crit] open %s/live\n", entry.BaseURL())
	}
	if !noOpen && !daemon.DaemonHasBrowser(entry) {
		launchLiveBrowser(entry.BaseURL()+"/live", openCmd)
	}
	runLiveClient(entry, key, quiet)
	return true
}

type liveCLIFlags struct {
	port                        int
	host                        string
	publicURL                   string
	allowUnauthenticatedNetwork bool
	noOpen                      bool
	quiet                       bool
	shareURL                    string
	cookieFlags                 stringSliceFlag
	cookieFile                  string
	cdpURL                      string
	origin                      string
	portlessService             string
}

func parseLiveCLIFlags(args []string) liveCLIFlags {
	fs := flag.NewFlagSet("live", flag.ExitOnError)
	port := fs.Int("port", 0, "Port to listen on")
	fs.IntVar(port, "p", 0, "Port (shorthand)")
	host := fs.String("host", "", "Host to listen on")
	publicURL := fs.String("public-url", "", "Advertised base URL (overrides CRIT_PUBLIC_URL)")
	allowUnauthNet := fs.Bool(config.AllowUnauthenticatedNetworkFlag, false, "Allow non-loopback listen or public_url without authentication")
	noOpen := fs.Bool("no-open", false, "Don't auto-open browser")
	quiet := fs.Bool("quiet", false, "On success, suppress connect/start status, tips, and session summary")
	fs.BoolVar(quiet, "q", false, "On success, suppress status (shorthand)")
	shareURL := fs.String("share-url", "", "Share service URL")
	var cookieFlags stringSliceFlag
	fs.Var(&cookieFlags, "cookie", "Cookie header value for upstream requests (repeatable)")
	cookieFile := fs.String("cookie-file", "", "File with upstream cookies (raw header or Netscape jar)")
	cdpURL := fs.String("cdp-url", "", "Chrome DevTools URL (e.g. http://127.0.0.1:9222) to reuse browser cookies")
	// Keep in sync with the fs.Bool/fs.BoolVar registrations above.
	args = clicmd.ReorderFlagsFirst(args, map[string]bool{
		config.AllowUnauthenticatedNetworkFlag: true,
		"no-open":                              true,
		"quiet":                                true,
		"q":                                    true,
	})
	fs.Parse(args)

	positional := fs.Args()
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: crit live <url>")
		os.Exit(1)
	}
	rawURL := positional[0]
	portlessService := ""
	if rawURL == "portless" {
		if len(positional) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: crit live portless <service>")
			os.Exit(1)
		}
		portlessService = positional[1]
		rawURL = ""
	} else {
		var err error
		rawURL, err = normalizeLiveURL(rawURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "crit live: %v\n", err)
			os.Exit(1)
		}
	}
	return liveCLIFlags{
		port:                        *port,
		host:                        *host,
		publicURL:                   *publicURL,
		allowUnauthenticatedNetwork: *allowUnauthNet,
		noOpen:                      *noOpen,
		quiet:                       *quiet,
		shareURL:                    *shareURL,
		cookieFlags:                 cookieFlags,
		cookieFile:                  *cookieFile,
		cdpURL:                      *cdpURL,
		origin:                      rawURL,
		portlessService:             portlessService,
	}
}

func normalizeLiveURL(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not a valid http/https URL", rawURL)
	}
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimSuffix(u.String(), "/"), nil
}

func defaultLookupPortlessServiceURL(service string) (string, error) {
	path, err := exec.LookPath("portless")
	if err != nil {
		return "", fmt.Errorf("portless executable not found in PATH")
	}
	cmd := exec.Command(path, "get", service)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if detail := strings.TrimSpace(string(exitErr.Stderr)); detail != "" {
				return "", fmt.Errorf("portless get %q: %s", service, detail)
			}
		}
		return "", fmt.Errorf("portless get %q: %w", service, err)
	}
	return string(output), nil
}

func resolveLiveOrigin(f *liveCLIFlags) error {
	if f.portlessService == "" {
		return nil
	}
	rawURL, err := lookupPortlessServiceURL(f.portlessService)
	if err != nil {
		return err
	}
	f.origin, err = normalizeLiveURL(rawURL)
	if err != nil {
		return fmt.Errorf("portless get %q returned %w", f.portlessService, err)
	}
	return nil
}

func buildLiveDaemonArgs(origin, liveCookies string, f liveCLIFlags, cfg config.Config, noOpenResolved bool) []string {
	daemonArgs := []string{"--live-origin", origin}
	if liveCookies != "" {
		daemonArgs = append(daemonArgs, "--live-cookie", liveCookies)
	}
	return daemon.AppendCommonDaemonFlags(daemonArgs, daemon.CommonDaemonFlags{
		Port:                        config.ResolvePort(f.port, cfg.Port),
		Host:                        config.ResolveHost(f.host, cfg.Host),
		PublicURL:                   config.ResolvePublicURL(f.publicURL, cfg),
		AllowUnauthenticatedNetwork: f.allowUnauthenticatedNetwork || config.EnvAllowsUnauthenticatedNetwork(),
		NoOpen:                      noOpenResolved,
		Quiet:                       f.quiet || cfg.Quiet,
		ShareURL:                    config.ResolveShareURL(f.shareURL, cfg, ""),
	})
}

// RunLive starts a live-mode review of a running web app.
func RunLive(args []string) {
	f := parseLiveCLIFlags(args)
	if err := resolveLiveOrigin(&f); err != nil {
		fmt.Fprintf(os.Stderr, "crit live portless: %v\n", err)
		os.Exit(1)
	}

	cwd, err := daemon.ResolvedCWD()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	cfg := config.LoadConfig(cwd)
	key := daemon.LiveSessionKey(cwd, f.origin)
	noOpenResolved := f.noOpen || cfg.NoOpen
	quiet := f.quiet || cfg.Quiet
	if connectToLiveDaemon(key, noOpenResolved, cfg.OpenCmd, quiet) {
		return
	}

	liveCookies, err := resolveLiveCookiesWithCDP(context.Background(), f.cookieFlags, f.cookieFile, f.cdpURL, cfg, cwd, f.origin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	checkLiveSmoke(f.origin, liveCookies)

	if connectToLiveDaemon(key, noOpenResolved, cfg.OpenCmd, quiet) {
		return
	}

	entry, err := startLiveDaemon(key, buildLiveDaemonArgs(f.origin, liveCookies, f, cfg, noOpenResolved))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not start live daemon: %v\n", err)
		os.Exit(1)
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "[crit] starting daemon on :%d (api), :%d (proxy)\n",
			entry.Port, entry.Port+1)
		fmt.Fprintf(os.Stderr, "[crit] open %s/live\n", entry.BaseURL())
	}

	installLiveDaemonSignalHandler(entry.PID)

	runLiveClient(entry, key, quiet)
}

func checkLiveSmoke(origin, cookies string) {
	result := runSmokeTest(origin, cookies)
	switch result.kind {
	case smokeConnRefused, smokeNonHTML:
		fmt.Fprintf(os.Stderr, "Error: %s\n", result.message)
		os.Exit(1)
	case smokeNon2xx, smokeMissingBody:
		fmt.Fprintf(os.Stderr, "[crit] warning: %s\n", result.message)
	}
	if result.hasCSPFrameAncestors {
		fmt.Fprintf(os.Stderr, "[crit] note: upstream has frame-ancestors CSP; stripped by proxy\n")
	}
	for _, n := range result.frameworkNotes {
		fmt.Fprintf(os.Stderr, "[crit] note: %s\n", n)
	}
}

func installDaemonSignalHandler(pid int) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		proc, err := os.FindProcess(pid)
		if err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
		os.Exit(0)
	}()
}
