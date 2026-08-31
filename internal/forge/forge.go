// Package forge defines the hosting-provider boundary used by Crit's remote
// review integrations. It deliberately contains only normalized data and an
// interface; provider-specific API payloads stay in internal/github and
// internal/gitlab.
package forge

import (
	"context"
	"errors"
)

// Kind identifies a supported code-hosting provider.
type Kind string

const (
	Auto   Kind = "auto"
	GitHub Kind = "github"
	GitLab Kind = "gitlab"
)

// ChangeID identifies a pull or merge request. Number is the repository-local
// PR number / MR IID. Project and Host are populated when a URL points at a
// project other than the current checkout.
type ChangeID struct {
	Number  int
	Project string
	Host    string
}

// RemoteRef identifies a provider-owned review comment. GitHub uses only
// CommentID; GitLab additionally requires ThreadID for note mutations.
type RemoteRef struct {
	Forge        Kind   `json:"forge"`
	ChangeNumber int    `json:"change_number,omitempty"`
	CommentID    int64  `json:"comment_id"`
	ThreadID     string `json:"thread_id,omitempty"`
}

// RepoContext describes the repository against which a provider command runs.
type RepoContext struct {
	Root    string
	Remote  string
	Project string
	Host    string
}

// RepoRef identifies a repository that owns one side of a change request.
type RepoRef struct {
	Project  string
	Host     string
	CloneURL string
}

// ChangeRequest is the provider-neutral metadata Crit needs for focus mode.
type ChangeRequest struct {
	ID              ChangeID
	URL             string
	Title           string
	Body            string
	State           string
	Draft           bool
	BaseRefName     string
	HeadRefName     string
	BaseSHA         string
	HeadSHA         string
	BaseRepo        RepoRef
	HeadRepo        RepoRef
	CrossRepository bool
	Additions       int
	Deletions       int
	ChangedFiles    int
	Author          string
	CreatedAt       string
}

// MergeMethod is a provider-neutral direct merge strategy.
type MergeMethod string

const (
	MergeMethodMerge  MergeMethod = "merge"
	MergeMethodSquash MergeMethod = "squash"
	MergeMethodRebase MergeMethod = "rebase"
)

// ChangeCheck is one CI check or commit status reported by a provider.
type ChangeCheck struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

// ChangeReview is the latest review from one reviewer.
type ChangeReview struct {
	Author string `json:"author"`
	State  string `json:"state"`
}

// ChangeStatus is the normalized merge readiness view consumed by the UI.
// ProviderID is an opaque API node ID used only by the provider during merge.
type ChangeStatus struct {
	ProviderID              string         `json:"-"`
	HeadSHA                 string         `json:"head_sha"`
	HeadCurrent             bool           `json:"head_current"`
	Checks                  []ChangeCheck  `json:"checks"`
	LatestReviews           []ChangeReview `json:"latest_reviews"`
	ReviewDecision          string         `json:"review_decision"`
	Mergeable               string         `json:"mergeable"`
	MergeStateStatus        string         `json:"merge_state_status"`
	UnresolvedReviewThreads int            `json:"unresolved_review_thread_count"`
	BaseRequiresMergeQueue  bool           `json:"base_requires_merge_queue"`
	Queued                  bool           `json:"queued"`
	AllowedMergeMethods     []MergeMethod  `json:"allowed_merge_methods"`
	DefaultMergeMethod      MergeMethod    `json:"default_merge_method,omitempty"`
	Ready                   bool           `json:"ready"`
	BlockingReasons         []string       `json:"blocking_reasons"`
}

// CommentScope preserves the active focused layer when importing comments.
type CommentScope struct {
	HeadSHA      string
	BaseSHA      string
	Forge        Kind
	ChangeNumber int
	DiffScope    string
}

type CommentSyncRequest struct {
	Repo       RepoContext
	Change     ChangeID
	ReviewPath string
	Scope      CommentScope
}

type CommentSyncResult struct {
	Added int `json:"added"`
}

type MergeRequest struct {
	Repo    RepoContext
	Change  ChangeID
	HeadSHA string
	Method  MergeMethod
}

type MergeResult struct {
	Queued bool `json:"queued"`
	Merged bool `json:"merged"`
}

var (
	ErrStaleHead           = errors.New("change request head changed")
	ErrNotReady            = errors.New("change request is not ready to merge")
	ErrMergeMethodRequired = errors.New("merge method is required")
	ErrMergeMethodDisabled = errors.New("merge method is not enabled")
)

// Optional capabilities keep status/sync/merge additive for providers that
// currently implement only the core Provider interface.
type StatusProvider interface {
	Status(ctx context.Context, repo RepoContext, id ChangeID) (ChangeStatus, error)
}

type CommentSyncProvider interface {
	SyncComments(ctx context.Context, request CommentSyncRequest) (CommentSyncResult, error)
}

type MergeProvider interface {
	Merge(ctx context.Context, request MergeRequest) (MergeResult, error)
}

type authenticationError interface {
	AuthenticationFailed() bool
}

func IsAuthenticationError(err error) bool {
	var authErr authenticationError
	return errors.As(err, &authErr) && authErr.AuthenticationFailed()
}

// ChangeSummary is the lightweight change-request shape used by the picker.
type ChangeSummary struct {
	ID          ChangeID `json:"id"`
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	HeadRefName string   `json:"head_ref_name"`
	HeadSHA     string   `json:"head_sha"`
	BaseRefName string   `json:"base_ref_name"`
	Draft       bool     `json:"draft"`
	Provider    Kind     `json:"provider"`
}

// PullRequest and PushRequest carry the already-tokenized CLI arguments while
// the existing GitHub implementation is migrated behind this boundary. The
// provider owns platform-specific validation and review-file persistence.
type PullRequest struct {
	Repo       RepoContext
	ChangeSpec string
	OutputDir  string
	Args       []string // compatibility path for direct provider callers
}

type PushRequest struct {
	Repo       RepoContext
	ChangeSpec string
	OutputDir  string
	DryRun     bool
	Message    string
	Event      string
	Args       []string // compatibility path for direct provider callers
}

type PullResult struct {
	Imported int
	Updated  int
	Skipped  int
	Warnings []string
}

type PushResult struct {
	Created  int
	Edited   int
	Deleted  int
	Replied  int
	Resolved int
	Warnings []string
}

// Provider is the complete remote-review boundary consumed by CLI, focus,
// picker, and remote-file loading code.
type Provider interface {
	Kind() Kind
	RequireAuth(ctx context.Context, repo RepoContext) error
	Detect(ctx context.Context, repo RepoContext) (ChangeID, error)
	Get(ctx context.Context, repo RepoContext, id ChangeID) (ChangeRequest, error)
	ListOpen(ctx context.Context, repo RepoContext) ([]ChangeSummary, error)
	Pull(ctx context.Context, request PullRequest) (PullResult, error)
	Push(ctx context.Context, request PushRequest) (PushResult, error)
	FetchFile(ctx context.Context, repo RepoContext, source RepoRef, sha, path string) ([]byte, error)
	Invalidate(id ChangeID)
}
