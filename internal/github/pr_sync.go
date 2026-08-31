package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// FetchPRCommentsContext fetches every inline review comment using the given
// repository directory and cancellation context.
func FetchPRCommentsContext(ctx context.Context, dir string, prNumber int) ([]GhComment, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	versionOut, versionErr := runCommandOutput(ctx, dir, "gh", "version")
	if versionErr == nil && ghVersionSupportsSlurp(string(versionOut)) {
		out, err := runCommandOutput(ctx, dir, "gh", "api",
			fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", prNumber),
			"--paginate", "--slurp",
		)
		if err != nil {
			return nil, fmt.Errorf("fetching pull request comments: %w", err)
		}
		var pages [][]ghComment
		if err := json.Unmarshal(out, &pages); err != nil {
			return nil, fmt.Errorf("parsing pull request comments: %w", err)
		}
		var comments []ghComment
		for _, page := range pages {
			comments = append(comments, page...)
		}
		return comments, nil
	}

	out, err := runCommandOutput(ctx, dir, "gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", prNumber),
		"--paginate", "--jq", ".[]",
	)
	if err != nil {
		return nil, fmt.Errorf("fetching pull request comments: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	var comments []ghComment
	for {
		var comment ghComment
		if err := decoder.Decode(&comment); err != nil {
			if errors.Is(err, io.EOF) {
				return comments, nil
			}
			return nil, fmt.Errorf("parsing pull request comments: %w", err)
		}
		comments = append(comments, comment)
	}
}

// FetchPRThreadResolvedContext returns GitHub's resolution state for every
// review-thread comment while also honoring the configured project directory.
func FetchPRThreadResolvedContext(ctx context.Context, dir string, prNumber int) (map[int64]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	repoOut, err := runCommandOutput(ctx, dir, "gh", "repo", "view", "--json", "owner,name")
	if err != nil {
		return nil, fmt.Errorf("resolving current repository: %w", err)
	}
	var repo repoMergeRaw
	if err := json.Unmarshal(repoOut, &repo); err != nil {
		return nil, fmt.Errorf("parsing current repository: %w", err)
	}
	resolved, _, err := fetchPRThreadStateContext(ctx, dir, repo.Owner.Login, repo.Name, prNumber)
	return resolved, err
}

func fetchPRThreadStateContext(ctx context.Context, dir, owner, name string, prNumber int) (map[int64]bool, int, error) {
	resolved := make(map[int64]bool)
	unresolvedCount := 0
	cursor := ""
	for {
		page, next, unresolved, err := fetchPRThreadStatePageContext(ctx, dir, owner, name, prNumber, cursor)
		if err != nil {
			return nil, 0, err
		}
		for id, state := range page {
			resolved[id] = state
		}
		unresolvedCount += unresolved
		if next == "" {
			return resolved, unresolvedCount, nil
		}
		cursor = next
	}
}

func fetchPRThreadStatePageContext(ctx context.Context, dir, owner, name string, prNumber int, cursor string) (map[int64]bool, string, int, error) {
	const query = `query($owner:String!,$name:String!,$pr:Int!,$after:String){
  repository(owner:$owner, name:$name){
    pullRequest(number:$pr){
      reviewThreads(first:100, after:$after){
        pageInfo{ hasNextPage endCursor }
        nodes{ isResolved comments(first:100){ nodes{ databaseId } } }
      }
    }
  }
}`
	args := []string{"api", "graphql",
		"-f", "owner=" + owner,
		"-f", "name=" + name,
		"-F", "pr=" + strconv.Itoa(prNumber),
		"-f", "query=" + query,
	}
	if cursor != "" {
		args = append(args, "-f", "after="+cursor)
	}
	out, err := runCommandOutput(ctx, dir, "gh", args...)
	if err != nil {
		return nil, "", 0, fmt.Errorf("fetching pull request review threads: %w", err)
	}
	var response graphqlThreadResp
	if err := json.Unmarshal(out, &response); err != nil {
		return nil, "", 0, fmt.Errorf("parsing pull request review threads: %w", err)
	}
	threads := response.Data.Repository.PullRequest.ReviewThreads
	page := make(map[int64]bool)
	unresolved := 0
	for _, thread := range threads.Nodes {
		if !thread.IsResolved {
			unresolved++
		}
		for _, comment := range thread.Comments.Nodes {
			if comment.DatabaseID != 0 {
				page[comment.DatabaseID] = thread.IsResolved
			}
		}
	}
	next := ""
	if threads.PageInfo.HasNextPage {
		next = threads.PageInfo.EndCursor
	}
	return page, next, unresolved, nil
}
