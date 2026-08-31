package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tomasz-tomczyk/crit/internal/forge"
)

// MergeMethod is one of GitHub's direct pull-request merge strategies.
type MergeMethod = forge.MergeMethod

const (
	MergeMethodMerge  = forge.MergeMethodMerge
	MergeMethodSquash = forge.MergeMethodSquash
	MergeMethodRebase = forge.MergeMethodRebase
)

// PRCheck is one check or commit status in GitHub's status rollup.
type PRCheck = forge.ChangeCheck

// PRReview is the latest review from one reviewer.
type PRReview = forge.ChangeReview

// PRStatus is a fresh, merge-oriented view of the current GitHub pull request.
type PRStatus = forge.ChangeStatus

// MergeResult describes whether GitHub accepted a direct merge or a queue entry.
type MergeResult = forge.MergeResult

var (
	ErrStaleHead           = forge.ErrStaleHead
	ErrNotReady            = forge.ErrNotReady
	ErrMergeMethodRequired = forge.ErrMergeMethodRequired
	ErrMergeMethodDisabled = forge.ErrMergeMethodDisabled
)

type commandFailure struct {
	command string
	output  string
	err     error
}

func (e *commandFailure) Error() string {
	if e.output == "" {
		return fmt.Sprintf("%s: %v", e.command, e.err)
	}
	return fmt.Sprintf("%s: %v: %s", e.command, e.err, e.output)
}

func (e *commandFailure) Unwrap() error { return e.err }

func (e *commandFailure) AuthenticationFailed() bool {
	out := strings.ToLower(e.output)
	return strings.Contains(out, "http 401") ||
		strings.Contains(out, "bad credentials") ||
		strings.Contains(out, "authentication required") ||
		strings.Contains(out, "gh auth login")
}

var runCommandOutput = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, &commandFailure{
			command: strings.Join(append([]string{name}, args...), " "),
			output:  strings.TrimSpace(string(out)),
			err:     err,
		}
	}
	return out, nil
}

// IsAuthenticationError reports whether gh rejected the current credentials.
func IsAuthenticationError(err error) bool {
	return forge.IsAuthenticationError(err)
}

type prStatusRaw struct {
	ID                string    `json:"id"`
	HeadRefOid        string    `json:"headRefOid"`
	BaseRefName       string    `json:"baseRefName"`
	State             string    `json:"state"`
	IsDraft           bool      `json:"isDraft"`
	ReviewDecision    string    `json:"reviewDecision"`
	Mergeable         string    `json:"mergeable"`
	MergeStateStatus  string    `json:"mergeStateStatus"`
	AutoMergeRequest  *struct{} `json:"autoMergeRequest"`
	StatusCheckRollup []struct {
		Type       string `json:"__typename"`
		Name       string `json:"name"`
		Context    string `json:"context"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
		DetailsURL string `json:"detailsUrl"`
		TargetURL  string `json:"targetUrl"`
	} `json:"statusCheckRollup"`
	LatestReviews []struct {
		Author prAuthor `json:"author"`
		State  string   `json:"state"`
	} `json:"latestReviews"`
}

type repoMergeRaw struct {
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	Name                     string `json:"name"`
	MergeCommitAllowed       bool   `json:"mergeCommitAllowed"`
	SquashMergeAllowed       bool   `json:"squashMergeAllowed"`
	RebaseMergeAllowed       bool   `json:"rebaseMergeAllowed"`
	ViewerDefaultMergeMethod string `json:"viewerDefaultMergeMethod"`
}

type activeBranchRule struct {
	Type       string `json:"type"`
	Parameters struct {
		AllowedMergeMethods []string `json:"allowed_merge_methods"`
	} `json:"parameters"`
}

const prStatusJSONFields = "id,headRefOid,baseRefName,state,isDraft,statusCheckRollup,latestReviews,reviewDecision,mergeable,mergeStateStatus,autoMergeRequest"
const repoMergeJSONFields = "owner,name,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed,viewerDefaultMergeMethod"

// FetchPRStatus refreshes all merge gates for one explicitly selected PR.
func FetchPRStatus(ctx context.Context, dir string, prNumber int) (PRStatus, error) { //nolint:gocyclo
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var raw prStatusRaw
	out, err := runCommandOutput(ctx, dir, "gh", "pr", "view", strconv.Itoa(prNumber), "--json", prStatusJSONFields)
	if err != nil {
		return PRStatus{}, fmt.Errorf("refreshing pull request: %w", err)
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return PRStatus{}, fmt.Errorf("parsing pull request status: %w", err)
	}

	status := statusFromRaw(raw)
	headOut, headErr := runCommandOutput(ctx, dir, "git", "rev-parse", "HEAD")
	if headErr == nil {
		status.HeadCurrent = strings.TrimSpace(string(headOut)) == status.HeadSHA
	}

	var repo repoMergeRaw
	repoOut, repoErr := runCommandOutput(ctx, dir, "gh", "repo", "view", "--json", repoMergeJSONFields)
	if repoErr != nil && !IsAuthenticationError(repoErr) {
		// Older GHES schemas may not expose viewerDefaultMergeMethod. Keep all
		// policy discovery and simply omit the user's sticky default.
		const fallbackFields = "owner,name,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed"
		repoOut, repoErr = runCommandOutput(ctx, dir, "gh", "repo", "view", "--json", fallbackFields)
	}
	if repoErr == nil {
		repoErr = json.Unmarshal(repoOut, &repo)
		if repoErr != nil {
			repoErr = fmt.Errorf("parsing repository merge settings: %w", repoErr)
		}
	}

	var discoveryReasons []string
	if headErr != nil {
		discoveryReasons = append(discoveryReasons, "local HEAD is unavailable")
	}
	if repoErr != nil {
		discoveryReasons = append(discoveryReasons, "repository merge settings are unavailable")
	} else {
		status.AllowedMergeMethods = repoAllowedMergeMethods(repo)
		status.DefaultMergeMethod = normalizeMergeMethod(repo.ViewerDefaultMergeMethod)

		_, unresolved, threadErr := fetchPRThreadStateContext(ctx, dir, repo.Owner.Login, repo.Name, prNumber)
		if threadErr != nil {
			discoveryReasons = append(discoveryReasons, "review thread status is unavailable")
		} else {
			status.UnresolvedReviewThreads = unresolved
		}

		rules, rulesErr := fetchActiveBranchRules(ctx, dir, raw, repo)
		if rulesErr != nil {
			discoveryReasons = append(discoveryReasons, "merge queue requirements are unavailable")
		} else {
			applyActiveBranchRules(&status, rules)
			if status.BaseRequiresMergeQueue && raw.AutoMergeRequest != nil {
				status.Queued = true
			}
			if status.DefaultMergeMethod != "" && !containsMergeMethod(status.AllowedMergeMethods, status.DefaultMergeMethod) {
				status.DefaultMergeMethod = ""
			}
		}
	}

	deriveReady(&status, raw, discoveryReasons)
	return status, nil
}

func statusFromRaw(raw prStatusRaw) PRStatus {
	status := PRStatus{
		ProviderID:       raw.ID,
		HeadSHA:          raw.HeadRefOid,
		ReviewDecision:   strings.ToLower(raw.ReviewDecision),
		Mergeable:        strings.ToLower(raw.Mergeable),
		MergeStateStatus: strings.ToLower(raw.MergeStateStatus),
		Queued:           strings.EqualFold(raw.MergeStateStatus, "QUEUED"),
		Checks:           make([]PRCheck, 0, len(raw.StatusCheckRollup)),
		LatestReviews:    make([]PRReview, 0, len(raw.LatestReviews)),
	}
	for _, check := range raw.StatusCheckRollup {
		name := check.Name
		if name == "" {
			name = check.Context
		}
		state := check.Conclusion
		if check.Type == "StatusContext" {
			state = check.State
		} else if !strings.EqualFold(check.Status, "COMPLETED") {
			state = "PENDING"
		}
		if state == "" {
			state = check.Status
		}
		checkURL := check.DetailsURL
		if checkURL == "" {
			checkURL = check.TargetURL
		}
		status.Checks = append(status.Checks, PRCheck{Name: name, State: strings.ToLower(state), URL: checkURL})
	}
	for _, review := range raw.LatestReviews {
		status.LatestReviews = append(status.LatestReviews, PRReview{
			Author: displayName(review.Author.Login, review.Author.Name),
			State:  strings.ToLower(review.State),
		})
	}
	return status
}

func repoAllowedMergeMethods(repo repoMergeRaw) []MergeMethod {
	methods := make([]MergeMethod, 0, 3)
	if repo.MergeCommitAllowed {
		methods = append(methods, MergeMethodMerge)
	}
	if repo.SquashMergeAllowed {
		methods = append(methods, MergeMethodSquash)
	}
	if repo.RebaseMergeAllowed {
		methods = append(methods, MergeMethodRebase)
	}
	return methods
}

func normalizeMergeMethod(value string) MergeMethod {
	switch strings.ToLower(value) {
	case string(MergeMethodMerge):
		return MergeMethodMerge
	case string(MergeMethodSquash):
		return MergeMethodSquash
	case string(MergeMethodRebase):
		return MergeMethodRebase
	default:
		return ""
	}
}

func fetchActiveBranchRules(ctx context.Context, dir string, raw prStatusRaw, repo repoMergeRaw) ([]activeBranchRule, error) {
	// The active-rules endpoint has already evaluated inheritance, conditions,
	// and enforcement for this branch, unlike the repository ruleset listing.
	branch := url.PathEscape(rawBranchName(raw))
	endpoint := fmt.Sprintf("repos/%s/%s/rules/branches/%s", repo.Owner.Login, repo.Name, branch)
	out, err := runCommandOutput(ctx, dir, "gh", "api", endpoint)
	if err != nil {
		return nil, fmt.Errorf("fetching active branch rules: %w", err)
	}
	var rules []activeBranchRule
	if err := json.Unmarshal(out, &rules); err != nil {
		return nil, fmt.Errorf("parsing active branch rules: %w", err)
	}
	return rules, nil
}

// rawBranchName is populated separately by FetchPRStatus before rules discovery.
func rawBranchName(raw prStatusRaw) string { return raw.BaseRefName }

func applyActiveBranchRules(status *PRStatus, rules []activeBranchRule) {
	for _, rule := range rules {
		switch strings.ToLower(rule.Type) {
		case "merge_queue":
			status.BaseRequiresMergeQueue = true
		case "pull_request":
			if rule.Parameters.AllowedMergeMethods != nil {
				allowed := make(map[MergeMethod]bool, len(rule.Parameters.AllowedMergeMethods))
				for _, method := range rule.Parameters.AllowedMergeMethods {
					allowed[normalizeMergeMethod(method)] = true
				}
				filtered := status.AllowedMergeMethods[:0]
				for _, method := range status.AllowedMergeMethods {
					if allowed[method] {
						filtered = append(filtered, method)
					}
				}
				status.AllowedMergeMethods = filtered
			}
		}
	}
}

func deriveReady(status *PRStatus, raw prStatusRaw, discoveryReasons []string) {
	reasons := append([]string(nil), discoveryReasons...)
	if !strings.EqualFold(raw.State, "OPEN") {
		reasons = append(reasons, "pull request is not open")
	}
	if raw.IsDraft {
		reasons = append(reasons, "pull request is a draft")
	}
	if !status.HeadCurrent {
		reasons = append(reasons, "local HEAD does not match the pull request head")
	}
	for _, check := range status.Checks {
		switch check.State {
		case "success", "neutral", "skipped":
		default:
			reasons = append(reasons, "checks have not completed successfully")
			goto reviews
		}
	}

reviews:
	if status.ReviewDecision == "changes_requested" || status.ReviewDecision == "review_required" {
		reasons = append(reasons, "required reviews are not satisfied")
	} else {
		for _, review := range status.LatestReviews {
			if review.State == "changes_requested" {
				reasons = append(reasons, "changes were requested")
				break
			}
		}
	}
	if status.UnresolvedReviewThreads > 0 {
		reasons = append(reasons, fmt.Sprintf("%d review threads are unresolved", status.UnresolvedReviewThreads))
	}
	if status.Mergeable != "mergeable" {
		reasons = append(reasons, "GitHub does not report the pull request as mergeable")
	}
	switch status.MergeStateStatus {
	case "blocked", "dirty", "unknown", "":
		reasons = append(reasons, "GitHub merge state is "+fallback(status.MergeStateStatus, "unknown"))
	}
	if !status.BaseRequiresMergeQueue && len(status.AllowedMergeMethods) == 0 {
		reasons = append(reasons, "no direct merge methods are enabled")
	}
	status.BlockingReasons = uniqueStrings(reasons)
	status.Ready = len(status.BlockingReasons) == 0
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func fallback(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

// MergePR refreshes status, validates the expected head and policy gates, then
// asks gh to enqueue or directly merge without any administrative bypasses.
func MergePR(ctx context.Context, dir string, prNumber int, expectedHead string, method MergeMethod) (MergeResult, error) { //nolint:gocyclo
	status, err := FetchPRStatus(ctx, dir, prNumber)
	if err != nil {
		return MergeResult{}, err
	}
	if expectedHead == "" || expectedHead != status.HeadSHA {
		return MergeResult{}, fmt.Errorf("%w: current head is %s", ErrStaleHead, status.HeadSHA)
	}
	if status.BaseRequiresMergeQueue && status.Queued {
		return MergeResult{Queued: true}, nil
	}
	if !status.Ready {
		return MergeResult{}, fmt.Errorf("%w: %s", ErrNotReady, strings.Join(status.BlockingReasons, "; "))
	}

	if status.BaseRequiresMergeQueue {
		if status.ProviderID == "" {
			return MergeResult{}, errors.New("enqueueing pull request: GitHub node ID is unavailable")
		}
		const mutation = `mutation($id:ID!,$head:GitObjectID!){
  enqueuePullRequest(input:{pullRequestId:$id,expectedHeadOid:$head}){
    mergeQueueEntry{id}
  }
}`
		enqueueOut, enqueueErr := runCommandOutput(ctx, dir, "gh", "api", "graphql",
			"-f", "id="+status.ProviderID,
			"-f", "head="+status.HeadSHA,
			"-f", "query="+mutation,
		)
		if enqueueErr == nil {
			var response struct {
				Data struct {
					EnqueuePullRequest struct {
						MergeQueueEntry struct {
							ID string `json:"id"`
						} `json:"mergeQueueEntry"`
					} `json:"enqueuePullRequest"`
				} `json:"data"`
			}
			if err := json.Unmarshal(enqueueOut, &response); err != nil {
				enqueueErr = fmt.Errorf("parsing enqueue response: %w", err)
			} else if response.Data.EnqueuePullRequest.MergeQueueEntry.ID == "" {
				enqueueErr = errors.New("GitHub did not return a merge queue entry")
			}
		}
		if enqueueErr != nil {
			refreshed, refreshErr := FetchPRStatus(ctx, dir, prNumber)
			if refreshErr == nil && refreshed.HeadSHA == status.HeadSHA && refreshed.Queued {
				return MergeResult{Queued: true}, nil
			}
			return MergeResult{}, fmt.Errorf("enqueueing pull request: %w", enqueueErr)
		}
		return MergeResult{Queued: true}, nil
	}

	args := []string{"pr", "merge", strconv.Itoa(prNumber), "--match-head-commit", status.HeadSHA}
	if method == "" {
		if len(status.AllowedMergeMethods) != 1 {
			return MergeResult{}, ErrMergeMethodRequired
		}
		method = status.AllowedMergeMethods[0]
	}
	if !containsMergeMethod(status.AllowedMergeMethods, method) {
		return MergeResult{}, fmt.Errorf("%w: %s", ErrMergeMethodDisabled, method)
	}
	args = append(args, "--"+string(method))
	if _, err := runCommandOutput(ctx, dir, "gh", args...); err != nil {
		return MergeResult{}, fmt.Errorf("merging pull request: %w", err)
	}
	return MergeResult{Merged: true}, nil
}

func containsMergeMethod(methods []MergeMethod, method MergeMethod) bool {
	index := sort.Search(len(methods), func(i int) bool { return methods[i] >= method })
	if index < len(methods) && methods[index] == method {
		return true
	}
	// Repository order is stable but rule intersections need not remain sorted.
	for _, allowed := range methods {
		if allowed == method {
			return true
		}
	}
	return false
}
