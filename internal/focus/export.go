package focus

import (
	"github.com/tomasz-tomczyk/crit/internal/forge"
	"github.com/tomasz-tomczyk/crit/internal/vcs"
)

// LooksLikePRURL reports whether spec is a GitHub pull request URL.
func LooksLikePRURL(spec string) bool {
	return prURLRe.MatchString(spec)
}

// LooksLikeMRURL reports whether spec is a GitLab merge request URL.
func LooksLikeMRURL(spec string) bool {
	return mrURLRe.MatchString(spec)
}

// ParsePRSpec resolves --pr <num|url> to a numeric PR number.
func ParsePRSpec(spec string) (int, error) {
	return parsePRSpec(spec)
}

// ParseRangeSpec parses a base..head commit range.
func ParseRangeSpec(spec string) (base, head string, err error) {
	return parseRangeSpec(spec)
}

// ParseScopeSpec normalizes a --scope flag value.
func ParseScopeSpec(s string) (DiffScope, error) {
	return parseScopeSpec(s)
}

// ResolveChangeFocus builds a fresh range focus from provider metadata.
func ResolveChangeFocus(change forge.ChangeRequest, provider forge.Kind, scope DiffScope, remoteFiles bool, v vcs.VCS, repoRoot string) (*Focus, error) {
	labelKind := "PR"
	if provider == forge.GitLab {
		labelKind = "MR"
	}
	return resolveFocusFromChange(ChangeResolveInfo{
		URL:               change.URL,
		Number:            change.ID.Number,
		Title:             change.Title,
		BaseRefOid:        change.BaseSHA,
		HeadRefOid:        change.HeadSHA,
		BaseRefName:       change.BaseRefName,
		HeadRefName:       change.HeadRefName,
		HeadRepoURL:       change.HeadRepo.CloneURL,
		BaseRepoProject:   change.BaseRepo.Project,
		HeadRepoProject:   change.HeadRepo.Project,
		HeadRepoHost:      change.HeadRepo.Host,
		IsCrossRepository: change.CrossRepository,
	}, string(provider), labelKind, scope, remoteFiles, v, repoRoot)
}

// CommentScopeOverrideFromFlag normalizes the raw --scope string for crit comment.
func CommentScopeOverrideFromFlag(s string) (CommentFocusOverride, error) {
	return commentScopeOverrideFromFlag(s)
}

// SetProbeDaemonFocusFnForTest replaces daemon focus probing during tests.
func SetProbeDaemonFocusFnForTest(fn func() *Focus) (restore func()) {
	prev := probeDaemonFocusFn
	if fn == nil {
		probeDaemonFocusFn = func() *Focus { return nil }
	} else {
		probeDaemonFocusFn = fn
	}
	return func() { probeDaemonFocusFn = prev }
}
