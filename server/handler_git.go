package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/changes"
	"github.com/adrianliechti/wingman-agent/server/ahp"
)

func (s *Server) handleWorkspaceGit(w http.ResponseWriter, r *http.Request) {
	if !s.Project.workspace.HasChanges() {
		writeJSON(w, nil)
		return
	}
	result, err := s.Project.gitStatus(r.Context())
	writeGitResult(w, result, err)
}

// HTTP commands remain deliberately small. File edits and layer mutations use AHP.
func (s *Server) handleGitBranches(w http.ResponseWriter, r *http.Request) {
	result, err := s.Project.gitBranches(r.Context(), false)
	writeGitResult(w, result, err)
}
func (s *Server) handleGitFetch(w http.ResponseWriter, r *http.Request) {
	result, err := s.Project.gitBranches(r.Context(), true)
	writeGitResult(w, result, err)
}
func (s *Server) handleGitCommits(w http.ResponseWriter, r *http.Request) {
	result, err := s.Project.gitHistory(r.Context())
	writeGitResult(w, result, err)
}
func (s *Server) handleWorkspaceInsights(w http.ResponseWriter, r *http.Request) {
	result, err := s.Project.gitInsights(r.Context())
	writeGitResult(w, result, err)
}
func (s *Server) handleGitAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, gitRequestLimit))
		if err != nil {
			http.Error(w, "invalid Git request body", http.StatusBadRequest)
			return
		}
		if len(bytes.TrimSpace(data)) == 0 {
			data = []byte("{}")
		}
		var fields map[string]json.RawMessage
		if err := decodeGitParams(data, &fields); err != nil {
			writeGitResult(w, nil, err)
			return
		}
		if _, exists := fields["action"]; exists {
			writeGitResult(w, nil, ahp.InvalidParams("action is determined by the endpoint"))
			return
		}
		fields["action"], _ = json.Marshal(action)
		data, _ = json.Marshal(fields)
		p, err := decodeGitAction(data)
		if err != nil {
			writeGitResult(w, nil, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		result, err := s.Project.runGit(ctx, p)
		if err == nil {
			s.Project.refreshChangesets()
		}
		writeGitResult(w, result, err)
	}
}
func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	base, head, mode := strings.TrimSpace(query.Get("base")), strings.TrimSpace(query.Get("head")), query.Get("mode")
	if base == "" || head == "" {
		writeGitResult(w, nil, ahp.InvalidParams("base and head revisions are required"))
		return
	}
	if mode == "" {
		mode = "direct"
	}
	if mode != "direct" && mode != "merge-base" {
		writeGitResult(w, nil, ahp.InvalidParams("invalid compare mode"))
		return
	}
	comparison, err := s.Project.workspace.GitCompare(r.Context(), base, head, mode == "merge-base")
	if err != nil {
		writeGitResult(w, nil, err)
		return
	}
	channel, err := s.Project.comparisonChangeset(comparison.Diffs)
	if err != nil {
		writeGitResult(w, nil, err)
		return
	}
	writeJSON(w, map[string]string{"channel": channel, "base": base, "head": head, "base_hash": comparison.BaseHash,
		"head_hash": comparison.HeadHash, "merge_base_hash": comparison.MergeBaseHash})
}
func writeGitResult(w http.ResponseWriter, result any, err error) {
	if err == nil {
		writeJSON(w, result)
		return
	}
	status := http.StatusConflict
	var rpc *ahp.Error
	if errors.As(err, &rpc) && rpc.Code == -32602 {
		status = http.StatusBadRequest
	}
	if errors.Is(err, changes.ErrNotGitRepository) {
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}
