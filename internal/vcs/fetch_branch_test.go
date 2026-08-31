package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchRemoteBranchUpdatesTrackingRef(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin.git")
	source := filepath.Join(t.TempDir(), "source")
	local := filepath.Join(t.TempDir(), "local")
	runFetchBranchGit(t, "", "init", "--bare", origin)
	runFetchBranchGit(t, "", "init", "-b", "main", source)
	runFetchBranchGit(t, source, "config", "user.name", "Test User")
	runFetchBranchGit(t, source, "config", "user.email", "test@example.com")
	writeFetchBranchFile(t, source, "one\n")
	runFetchBranchGit(t, source, "add", "file.txt")
	runFetchBranchGit(t, source, "-c", "commit.gpgSign=false", "commit", "-m", "initial")
	runFetchBranchGit(t, source, "remote", "add", "origin", origin)
	runFetchBranchGit(t, source, "push", "-u", "origin", "main")
	runFetchBranchGit(t, "", "clone", origin, local)

	writeFetchBranchFile(t, source, "two\n")
	runFetchBranchGit(t, source, "add", "file.txt")
	runFetchBranchGit(t, source, "-c", "commit.gpgSign=false", "commit", "-m", "remote update")
	runFetchBranchGit(t, source, "push", "origin", "main")

	remoteRef, err := FetchRemoteBranch(local, "main")
	if err != nil {
		t.Fatal(err)
	}
	if remoteRef != "origin/main" {
		t.Fatalf("remote ref = %q, want origin/main", remoteRef)
	}
	want := strings.TrimSpace(runFetchBranchGit(t, source, "rev-parse", "HEAD"))
	got := strings.TrimSpace(runFetchBranchGit(t, local, "rev-parse", "origin/main"))
	if got != want {
		t.Fatalf("origin/main = %q, want %q", got, want)
	}
}

func TestFetchRemoteBranchRejectsNonBranchRef(t *testing.T) {
	if _, err := FetchRemoteBranch(t.TempDir(), "HEAD~1"); err == nil {
		t.Fatal("expected invalid branch error")
	}
}

func runFetchBranchGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFetchBranchFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
