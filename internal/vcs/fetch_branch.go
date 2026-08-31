package vcs

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// FetchRemoteBranch refreshes origin's tracking ref for a compare target.
func FetchRemoteBranch(repoRoot, ref string) (string, error) {
	branch := strings.TrimSpace(ref)
	branch = strings.TrimPrefix(branch, "refs/remotes/origin/")
	branch = strings.TrimPrefix(branch, "refs/heads/")
	branch = strings.TrimPrefix(branch, "origin/")
	if branch == "" {
		return "", fmt.Errorf("compare branch is required")
	}

	if _, err := runGit(context.Background(), repoRoot, "check-ref-format", "--branch", branch); err != nil {
		return "", fmt.Errorf("invalid compare branch %q", ref)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	refspec := "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
	if _, err := runGit(ctx, repoRoot, "fetch", "--no-tags", "origin", refspec); err != nil {
		return "", fmt.Errorf("fetching origin/%s: %w", branch, err)
	}
	return "origin/" + branch, nil
}
