package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tomasz-tomczyk/crit/internal/forge"
	"github.com/tomasz-tomczyk/crit/internal/review"
	"github.com/tomasz-tomczyk/crit/internal/session"
)

type fakeChangeProvider struct {
	status     forge.ChangeStatus
	statusErr  error
	merge      forge.MergeResult
	mergeErr   error
	mergeReq   forge.MergeRequest
	syncResult forge.CommentSyncResult
	syncErr    error
	syncFn     func(forge.CommentSyncRequest) error
}

func (*fakeChangeProvider) Kind() forge.Kind                                     { return forge.GitHub }
func (*fakeChangeProvider) RequireAuth(context.Context, forge.RepoContext) error { return nil }
func (*fakeChangeProvider) Detect(context.Context, forge.RepoContext) (forge.ChangeID, error) {
	return forge.ChangeID{}, nil
}
func (*fakeChangeProvider) Get(context.Context, forge.RepoContext, forge.ChangeID) (forge.ChangeRequest, error) {
	return forge.ChangeRequest{}, nil
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
func (*fakeChangeProvider) Invalidate(forge.ChangeID) {}
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
