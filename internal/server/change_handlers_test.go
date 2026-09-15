package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tomasz-tomczyk/crit/internal/forge"
	"github.com/tomasz-tomczyk/crit/internal/review"
	"github.com/tomasz-tomczyk/crit/internal/session"
	"github.com/tomasz-tomczyk/crit/internal/testutil"
	"github.com/tomasz-tomczyk/crit/internal/vcs"
)

type fakeChangeProvider struct {
	change     forge.ChangeRequest
	status     forge.ChangeStatus
	statusErr  error
	merge      forge.MergeResult
	mergeErr   error
	mergeReq   forge.MergeRequest
	syncResult forge.CommentSyncResult
	syncErr    error
	syncFn     func(forge.CommentSyncRequest) error
	calls      []string
}

func (*fakeChangeProvider) Kind() forge.Kind                                     { return forge.GitHub }
func (*fakeChangeProvider) RequireAuth(context.Context, forge.RepoContext) error { return nil }
func (*fakeChangeProvider) Detect(context.Context, forge.RepoContext) (forge.ChangeID, error) {
	return forge.ChangeID{}, nil
}
func (p *fakeChangeProvider) Get(context.Context, forge.RepoContext, forge.ChangeID) (forge.ChangeRequest, error) {
	p.calls = append(p.calls, "get")
	return p.change, nil
}
func (*fakeChangeProvider) ListOpen(context.Context, forge.RepoContext) ([]forge.ChangeSummary, error) {
	return nil, nil
}
func (*fakeChangeProvider) Pull(context.Context, forge.PullRequest) (forge.PullResult, error) {
	return forge.PullResult{}, nil
}
func (*fakeChangeProvider) Push(context.Context, forge.PushRequest) (forge.PushResult, error) {
	return forge.PushResult{}, nil
}
func (*fakeChangeProvider) FetchFile(context.Context, forge.RepoContext, forge.RepoRef, string, string) ([]byte, error) {
	return nil, nil
}
func (p *fakeChangeProvider) Invalidate(forge.ChangeID) { p.calls = append(p.calls, "invalidate") }
func (p *fakeChangeProvider) Status(context.Context, forge.RepoContext, forge.ChangeID) (forge.ChangeStatus, error) {
	return p.status, p.statusErr
}
func (p *fakeChangeProvider) SyncComments(_ context.Context, request forge.CommentSyncRequest) (forge.CommentSyncResult, error) {
	if p.syncFn != nil {
		if err := p.syncFn(request); err != nil {
			return forge.CommentSyncResult{}, err
		}
	}
	return p.syncResult, p.syncErr
}
func (p *fakeChangeProvider) Merge(_ context.Context, request forge.MergeRequest) (forge.MergeResult, error) {
	p.mergeReq = request
	return p.merge, p.mergeErr
}

type statusOnlyProvider struct{ *fakeChangeProvider }

func TestChangeStatusHTTP(t *testing.T) {
	s, _ := newTestServer(t)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/change/status", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("no-change status = %d, want 404", w.Code)
	}

	provider := &fakeChangeProvider{status: forge.ChangeStatus{
		HeadSHA: "abc", Ready: true, Checks: []forge.ChangeCheck{}, LatestReviews: []forge.ChangeReview{},
		AllowedMergeMethods: []forge.MergeMethod{forge.MergeMethodSquash}, BlockingReasons: []string{},
	}}
	s.SetChangeContext(provider, forge.RepoContext{Root: s.projectDir}, forge.ChangeID{Number: 42})
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/change/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var status forge.ChangeStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.HeadSHA != "abc" || !status.Ready {
		t.Fatalf("response = %+v", status)
	}
}

func TestChangeRefreshRebuildsFocusedDiff(t *testing.T) {
	dir := testutil.InitTestRepo(t)
	origin := t.TempDir()
	testutil.Git(t, origin, "init", "--bare")
	testutil.Git(t, dir, "remote", "add", "origin", origin)
	testutil.Git(t, dir, "push", "-u", "origin", "main")
	baseSHA := testutil.Git(t, dir, "rev-parse", "HEAD")
	testutil.Git(t, dir, "checkout", "-b", "feature")
	testutil.WriteFile(t, filepath.Join(dir, "review.txt"), "first\n")
	testutil.Git(t, dir, "add", "review.txt")
	testutil.Git(t, dir, "commit", "-m", "first version")
	testutil.Git(t, dir, "push", "-u", "origin", "feature")
	oldHeadSHA := testutil.Git(t, dir, "rev-parse", "HEAD")

	oldFocus := Focus{
		Kind:         FocusRange,
		Forge:        string(forge.GitHub),
		ChangeNumber: 42,
		PRURL:        "https://github.com/acme/repo/pull/42",
		BaseSHA:      baseSHA,
		HeadSHA:      oldHeadSHA,
		BaseRefName:  "main",
		HeadRefName:  "feature",
		DiffScope:    DiffScopeLayer,
	}
	comment := session.StampWithFocus(Comment{ID: "c1", StartLine: 1, EndLine: 1, Body: "keep me"}, oldFocus)
	sess := &Session{
		VCS:         &vcs.GitVCS{},
		Mode:        "git",
		Branch:      "feature",
		RepoRoot:    dir,
		ReviewRound: 1,
		Focus:       oldFocus,
		Files: []*FileEntry{{
			Path: "review.txt", AbsPath: filepath.Join(dir, "review.txt"), Status: "added",
			FileType: "code", Content: "first\n", Comments: []Comment{comment},
		}},
	}
	sess.InitTestChannels()

	agentRoot := t.TempDir()
	agentDir := filepath.Join(agentRoot, "agent")
	testutil.Git(t, agentRoot, "clone", "--branch", "feature", origin, agentDir)
	testutil.Git(t, agentDir, "config", "user.email", "agent@test.com")
	testutil.Git(t, agentDir, "config", "user.name", "Agent")
	testutil.WriteFile(t, filepath.Join(agentDir, "review.txt"), "first\nsecond\n")
	testutil.Git(t, agentDir, "add", "review.txt")
	testutil.Git(t, agentDir, "commit", "-m", "updated version")
	testutil.Git(t, agentDir, "push", "origin", "feature")
	newHeadSHA := testutil.Git(t, agentDir, "rev-parse", "HEAD")

	provider := &fakeChangeProvider{change: forge.ChangeRequest{
		ID: forge.ChangeID{Number: 42}, URL: oldFocus.PRURL, Title: "Updated PR",
		BaseRefName: "main", HeadRefName: "feature", BaseSHA: baseSHA, HeadSHA: newHeadSHA,
	}}
	s, err := NewServer(sess, frontendFS, "", false, "", "", "test", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	s.SetChangeContext(provider, forge.RepoContext{Root: dir}, forge.ChangeID{Number: 42})

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/change/refresh", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := provider.calls; len(got) != 2 || got[0] != "invalidate" || got[1] != "get" {
		t.Fatalf("provider calls = %v, want [invalidate get]", got)
	}
	sess.RLock()
	defer sess.RUnlock()
	if sess.Focus.HeadSHA != newHeadSHA {
		t.Fatalf("head SHA = %q, want %q", sess.Focus.HeadSHA, newHeadSHA)
	}
	if len(sess.Files) != 1 || sess.Files[0].Content != "first\nsecond\n" {
		t.Fatalf("refreshed files = %+v", sess.Files)
	}
	if len(sess.Files[0].Comments) != 1 || sess.Files[0].Comments[0].ID != "c1" {
		t.Fatalf("comments after refresh = %+v", sess.Files[0].Comments)
	}
}

func TestChangeMergeHTTP(t *testing.T) {
	s, _ := newTestServer(t)
	provider := &fakeChangeProvider{merge: forge.MergeResult{Merged: true}}
	s.SetChangeContext(provider, forge.RepoContext{Root: s.projectDir}, forge.ChangeID{Number: 42})

	w := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"head_sha":"abc","method":"squash"}`)
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/change/merge", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if provider.mergeReq.HeadSHA != "abc" || provider.mergeReq.Method != forge.MergeMethodSquash || provider.mergeReq.Change.Number != 42 {
		t.Fatalf("merge request = %+v", provider.mergeReq)
	}
}

func TestSyncChangeCommentsImmediatelyVisibleAndIdempotent(t *testing.T) {
	s, sess := newTestServer(t)
	restoreProbe := session.SetProbeDaemonFocusFnForTest(nil)
	t.Cleanup(restoreProbe)
	provider := &fakeChangeProvider{syncResult: forge.CommentSyncResult{Added: 1}}
	provider.syncFn = func(request forge.CommentSyncRequest) error {
		cj, err := review.LoadCritJSON(request.ReviewPath)
		if err != nil {
			return err
		}
		file := cj.Files["test.md"]
		for _, comment := range file.Comments {
			if comment.ID == "remote-1" {
				provider.syncResult.Added = 0
				return nil
			}
		}
		file.Comments = append(file.Comments, Comment{ID: "remote-1", StartLine: 1, EndLine: 1, Body: "remote"})
		cj.Files["test.md"] = file
		return review.SaveCritJSON(request.ReviewPath, cj)
	}
	s.SetChangeContext(provider, forge.RepoContext{Root: s.projectDir}, forge.ChangeID{Number: 42})

	for call, wantAdded := range []int{1, 0} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/change/comments/sync", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("call %d status = %d: %s", call+1, w.Code, w.Body.String())
		}
		var response forge.CommentSyncResult
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Added != wantAdded {
			t.Fatalf("call %d added = %d, want %d", call+1, response.Added, wantAdded)
		}
	}
	if comments := sess.GetComments("test.md"); len(comments) != 1 || comments[0].ID != "remote-1" {
		t.Fatalf("in-memory comments = %+v", comments)
	}
}

func TestSyncChangeCommentsHonorsReviewTypeGuard(t *testing.T) {
	s, sess := newTestServer(t)
	provider := &fakeChangeProvider{}
	s.SetChangeContext(provider, forge.RepoContext{Root: s.projectDir}, forge.ChangeID{Number: 42})
	sess.ReviewType = "live"

	_, err := s.SyncChangeComments(context.Background())
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("PR comment sync is not supported")) {
		t.Fatalf("error = %v", err)
	}
}

type unsupportedProvider struct{}

func (unsupportedProvider) Kind() forge.Kind                                     { return forge.GitLab }
func (unsupportedProvider) RequireAuth(context.Context, forge.RepoContext) error { return nil }
func (unsupportedProvider) Detect(context.Context, forge.RepoContext) (forge.ChangeID, error) {
	return forge.ChangeID{}, nil
}
func (unsupportedProvider) Get(context.Context, forge.RepoContext, forge.ChangeID) (forge.ChangeRequest, error) {
	return forge.ChangeRequest{}, nil
}
func (unsupportedProvider) ListOpen(context.Context, forge.RepoContext) ([]forge.ChangeSummary, error) {
	return nil, nil
}
func (unsupportedProvider) Pull(context.Context, forge.PullRequest) (forge.PullResult, error) {
	return forge.PullResult{}, nil
}
func (unsupportedProvider) Push(context.Context, forge.PushRequest) (forge.PushResult, error) {
	return forge.PushResult{}, nil
}
func (unsupportedProvider) FetchFile(context.Context, forge.RepoContext, forge.RepoRef, string, string) ([]byte, error) {
	return nil, nil
}
func (unsupportedProvider) Invalidate(forge.ChangeID) {}

func TestChangeCapabilityUnsupported(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetChangeContext(unsupportedProvider{}, forge.RepoContext{Root: s.projectDir}, forge.ChangeID{Number: 42})
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/change/status", nil),
		httptest.NewRequest(http.MethodPost, "/api/change/comments/sync", nil),
		httptest.NewRequest(http.MethodPost, "/api/change/merge", bytes.NewBufferString(`{"head_sha":"abc"}`)),
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, request)
		if w.Code != http.StatusNotImplemented {
			t.Errorf("%s status = %d, want 501: %s", request.URL.Path, w.Code, w.Body.String())
		}
	}
}

func TestChangeRoutesRejectWrongMethods(t *testing.T) {
	s, _ := newTestServer(t)
	provider := &fakeChangeProvider{}
	s.SetChangeContext(provider, forge.RepoContext{Root: s.projectDir}, forge.ChangeID{Number: 42})
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/change/status", nil),
		httptest.NewRequest(http.MethodGet, "/api/change/refresh", nil),
		httptest.NewRequest(http.MethodGet, "/api/change/comments/sync", nil),
		httptest.NewRequest(http.MethodGet, "/api/change/merge", nil),
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, request)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", request.URL.Path, w.Code)
		}
	}
}

var _ forge.Provider = (*fakeChangeProvider)(nil)
var _ forge.StatusProvider = (*fakeChangeProvider)(nil)
var _ forge.CommentSyncProvider = (*fakeChangeProvider)(nil)
var _ forge.MergeProvider = (*fakeChangeProvider)(nil)
var _ forge.Provider = unsupportedProvider{}
