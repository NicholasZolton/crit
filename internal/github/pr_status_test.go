package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func readyStatusFixture() prStatusRaw {
	return prStatusRaw{
		HeadRefOid:       "abc123",
		BaseRefName:      "main",
		State:            "OPEN",
		ReviewDecision:   "APPROVED",
		Mergeable:        "MERGEABLE",
		MergeStateStatus: "CLEAN",
		StatusCheckRollup: []struct {
			Type       string `json:"__typename"`
			Name       string `json:"name"`
			Context    string `json:"context"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
			DetailsURL string `json:"detailsUrl"`
			TargetURL  string `json:"targetUrl"`
		}{{Type: "CheckRun", Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", DetailsURL: "https://example.test/check"}},
	}
}

func TestStatusFromRawPreservesChecksAndReviews(t *testing.T) {
	raw := readyStatusFixture()
	review := struct {
		Author prAuthor `json:"author"`
		State  string   `json:"state"`
	}{Author: prAuthor{Login: "octocat", Name: "The Octocat"}, State: "APPROVED"}
	raw.LatestReviews = append(raw.LatestReviews, review)

	status := statusFromRaw(raw)
	if got, want := status.Checks, []PRCheck{{Name: "test", State: "success", URL: "https://example.test/check"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("checks = %#v, want %#v", got, want)
	}
	if got, want := status.LatestReviews, []PRReview{{Author: "The Octocat", State: "approved"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews = %#v, want %#v", got, want)
	}
}

func TestDeriveReadyAllSignals(t *testing.T) {
	raw := readyStatusFixture()
	status := statusFromRaw(raw)
	status.HeadCurrent = true
	status.AllowedMergeMethods = []MergeMethod{MergeMethodSquash}
	deriveReady(&status, raw, nil)
	if !status.Ready || len(status.BlockingReasons) != 0 {
		t.Fatalf("ready status = %+v", status)
	}

	status = statusFromRaw(raw)
	status.HeadCurrent = true
	status.AllowedMergeMethods = []MergeMethod{MergeMethodSquash}
	status.UnresolvedReviewThreads = 2
	status.Checks[0].State = "failure"
	raw.ReviewDecision = "CHANGES_REQUESTED"
	raw.Mergeable = "CONFLICTING"
	raw.MergeStateStatus = "DIRTY"
	status.ReviewDecision = "changes_requested"
	status.Mergeable = "conflicting"
	status.MergeStateStatus = "dirty"
	deriveReady(&status, raw, nil)
	if status.Ready {
		t.Fatal("blocked status was ready")
	}
	joined := strings.Join(status.BlockingReasons, "|")
	for _, reason := range []string{"checks", "reviews", "2 review threads", "mergeable", "dirty"} {
		if !strings.Contains(joined, reason) {
			t.Errorf("blocking reasons %q do not contain %q", joined, reason)
		}
	}
}

func TestMergePRQueuedOmitsMethodFlag(t *testing.T) {
	commands := stubReadyPRCommands(t, `[ {"type":"merge_queue"} ]`)
	result, err := MergePR(context.Background(), "/repo", 42, "abc123", MergeMethodSquash)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Queued || result.Merged {
		t.Fatalf("result = %+v", result)
	}
	last := (*commands)[len(*commands)-1]
	if !strings.Contains(last, "gh api graphql -f id=PR_kwDOtest -f head=abc123") {
		t.Fatalf("enqueue command = %q", last)
	}
	if strings.Contains(last, "--squash") {
		t.Fatalf("enqueue command included direct method: %q", last)
	}
}

func TestMergePRDirectUsesSelectedMethod(t *testing.T) {
	commands := stubReadyPRCommands(t, `[]`)
	result, err := MergePR(context.Background(), "/repo", 42, "abc123", MergeMethodSquash)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Merged || result.Queued {
		t.Fatalf("result = %+v", result)
	}
	last := (*commands)[len(*commands)-1]
	if last != "gh pr merge 42 --match-head-commit abc123 --squash" {
		t.Fatalf("merge command = %q", last)
	}
}

func TestMergePRRejectsStaleAndBlockedHeads(t *testing.T) {
	commands := stubReadyPRCommands(t, `[]`)
	_, err := MergePR(context.Background(), "/repo", 42, "old", MergeMethodSquash)
	if !errors.Is(err, ErrStaleHead) {
		t.Fatalf("error = %v, want ErrStaleHead", err)
	}
	if strings.Contains(strings.Join(*commands, "\n"), "gh pr merge") {
		t.Fatal("stale request ran gh pr merge")
	}

	commands = stubReadyPRCommands(t, `[]`)
	previous := runCommandOutput
	runCommandOutput = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		out, commandErr := previous(ctx, dir, name, args...)
		if name == "gh" && len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
			return []byte(`{"headRefOid":"abc123","baseRefName":"main","state":"OPEN","reviewDecision":"APPROVED","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","statusCheckRollup":[],"latestReviews":[]}`), nil
		}
		return out, commandErr
	}
	_, err = MergePR(context.Background(), "/repo", 42, "abc123", MergeMethodSquash)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("error = %v, want ErrNotReady", err)
	}
	if strings.Contains(strings.Join(*commands, "\n"), "gh pr merge") {
		t.Fatal("blocked request ran gh pr merge")
	}
}

func stubReadyPRCommands(t *testing.T, rules string) *[]string {
	t.Helper()
	previous := runCommandOutput
	commands := make([]string, 0)
	runCommandOutput = func(_ context.Context, dir, name string, args ...string) ([]byte, error) {
		if dir != "/repo" {
			t.Fatalf("command dir = %q", dir)
		}
		command := strings.Join(append([]string{name}, args...), " ")
		commands = append(commands, command)
		switch {
		case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view":
			return []byte(`{"id":"PR_kwDOtest","headRefOid":"abc123","baseRefName":"main","state":"OPEN","reviewDecision":"APPROVED","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}],"latestReviews":[]}`), nil
		case name == "git":
			return []byte("abc123\n"), nil
		case name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "view":
			return []byte(`{"owner":{"login":"owner"},"name":"repo","mergeCommitAllowed":true,"squashMergeAllowed":true,"rebaseMergeAllowed":true,"viewerDefaultMergeMethod":"SQUASH"}`), nil
		case strings.Contains(command, "api graphql") && strings.Contains(command, "reviewThreads"):
			return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`), nil
		case strings.Contains(command, "api graphql") && strings.Contains(command, "enqueuePullRequest"):
			return []byte(`{"data":{"enqueuePullRequest":{"mergeQueueEntry":{"id":"MQE_test"}}}}`), nil
		case strings.Contains(command, "api repos/owner/repo/rules/branches/main"):
			return []byte(rules), nil
		case strings.Contains(command, "gh pr merge"):
			return []byte("ok"), nil
		default:
			t.Fatalf("unexpected command: %s", command)
			return nil, nil
		}
	}
	t.Cleanup(func() { runCommandOutput = previous })
	return &commands
}
