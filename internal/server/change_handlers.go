package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tomasz-tomczyk/crit/internal/forge"
	"github.com/tomasz-tomczyk/crit/internal/review"
	"github.com/tomasz-tomczyk/crit/internal/session"
)

var errNoChangeForSession = errors.New("no change request for this session")

func (s *Server) currentChange() (forge.Provider, forge.RepoContext, forge.ChangeID, bool) {
	s.prInfoMu.RLock()
	defer s.prInfoMu.RUnlock()
	if s.changeProvider == nil || s.changeID.Number <= 0 {
		return nil, forge.RepoContext{}, forge.ChangeID{}, false
	}
	return s.changeProvider, s.changeRepo, s.changeID, true
}

func (s *Server) handleChangeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provider, repo, id, ok := s.currentChange()
	if !ok {
		http.Error(w, errNoChangeForSession.Error(), http.StatusNotFound)
		return
	}
	statusProvider, ok := provider.(forge.StatusProvider)
	if !ok {
		s.writeUnsupportedCapability(w, provider, "status")
		return
	}
	status, err := statusProvider.Status(r.Context(), repo, id)
	if err != nil {
		s.writeForgeError(w, err)
		return
	}
	writeJSON(w, status)
}

func (s *Server) handleChangeCommentsSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := s.SyncChangeComments(r.Context())
	if err != nil {
		if errors.Is(err, errNoChangeForSession) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		var unsupported *unsupportedCapabilityError
		if errors.As(err, &unsupported) {
			http.Error(w, err.Error(), http.StatusNotImplemented)
			return
		}
		s.writeForgeError(w, err)
		return
	}
	writeJSON(w, result)
}

// SyncChangeComments imports comments through the active provider capability
// and immediately refreshes the daemon's in-memory comments.
func (s *Server) SyncChangeComments(ctx context.Context) (forge.CommentSyncResult, error) {
	provider, repo, id, ok := s.currentChange()
	if !ok {
		return forge.CommentSyncResult{}, errNoChangeForSession
	}
	syncProvider, ok := provider.(forge.CommentSyncProvider)
	if !ok {
		return forge.CommentSyncResult{}, &unsupportedCapabilityError{provider: provider.Kind(), capability: "comment sync"}
	}
	sess := s.session.Load()
	if sess == nil {
		return forge.CommentSyncResult{}, errors.New("session is not ready")
	}
	if err := checkForgeSyncAllowed(session.CritJSON{ReviewType: sess.ReviewType}, "PR comment sync"); err != nil {
		return forge.CommentSyncResult{}, err
	}
	if err := sess.SyncWriteFiles(); err != nil {
		return forge.CommentSyncResult{}, err
	}
	critPath := sess.CritJSONPath()
	cj, err := review.LoadCritJSON(critPath)
	if err != nil {
		return forge.CommentSyncResult{}, err
	}
	// Preserve the existing provenance guard before any provider can merge.
	if err := checkForgeSyncAllowed(cj, "PR comment sync"); err != nil {
		return forge.CommentSyncResult{}, err
	}
	scope := session.ResolvePullScope(&cj)
	result, err := syncProvider.SyncComments(ctx, forge.CommentSyncRequest{
		Repo:       repo,
		Change:     id,
		ReviewPath: critPath,
		Scope: forge.CommentScope{
			HeadSHA:      scope.HeadSHA,
			BaseSHA:      scope.BaseSHA,
			Forge:        forge.Kind(scope.Forge),
			ChangeNumber: scope.ChangeNumber,
			DiffScope:    scope.DiffScope,
		},
	})
	if err != nil {
		return forge.CommentSyncResult{}, err
	}
	sess.SyncCommentsFromDisk()
	return result, nil
}

func (s *Server) handleChangeMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provider, repo, id, ok := s.currentChange()
	if !ok {
		http.Error(w, errNoChangeForSession.Error(), http.StatusNotFound)
		return
	}
	mergeProvider, ok := provider.(forge.MergeProvider)
	if !ok {
		s.writeUnsupportedCapability(w, provider, "merge")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var request struct {
		HeadSHA string            `json:"head_sha"`
		Method  forge.MergeMethod `json:"method"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.HeadSHA) == "" {
		http.Error(w, "invalid merge request", http.StatusBadRequest)
		return
	}
	switch request.Method {
	case "", forge.MergeMethodMerge, forge.MergeMethodSquash, forge.MergeMethodRebase:
	default:
		http.Error(w, "invalid merge method", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	result, err := mergeProvider.Merge(ctx, forge.MergeRequest{
		Repo: repo, Change: id, HeadSHA: request.HeadSHA, Method: request.Method,
	})
	if err != nil {
		switch {
		case errors.Is(err, forge.ErrStaleHead), errors.Is(err, forge.ErrNotReady):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, forge.ErrMergeMethodRequired), errors.Is(err, forge.ErrMergeMethodDisabled):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			s.writeForgeError(w, err)
		}
		return
	}
	writeJSON(w, result)
}

type unsupportedCapabilityError struct {
	provider   forge.Kind
	capability string
}

func (e *unsupportedCapabilityError) Error() string {
	return string(e.provider) + " does not support change " + e.capability
}

func (s *Server) writeUnsupportedCapability(w http.ResponseWriter, provider forge.Provider, capability string) {
	http.Error(w, (&unsupportedCapabilityError{provider: provider.Kind(), capability: capability}).Error(), http.StatusNotImplemented)
}

func (s *Server) writeForgeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	if forge.IsAuthenticationError(err) {
		w.WriteHeader(http.StatusUnauthorized)
	} else {
		w.WriteHeader(http.StatusBadGateway)
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
