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
)

type GitFileStatus struct {
	Path           string `json:"path"`
	OriginalPath   string `json:"original_path,omitempty"`
	IndexStatus    string `json:"index_status,omitempty"`
	WorktreeStatus string `json:"worktree_status,omitempty"`
	Staged         bool   `json:"staged"`
	Changed        bool   `json:"changed"`
	Conflict       bool   `json:"conflict,omitempty"`
}

type GitStatus struct {
	Branch    string          `json:"branch"`
	Head      string          `json:"head"`
	Upstream  string          `json:"upstream,omitempty"`
	Ahead     int             `json:"ahead"`
	Behind    int             `json:"behind"`
	HasRemote bool            `json:"has_remote"`
	Files     []GitFileStatus `json:"files"`
}

type GitBranch struct {
	Name    string `json:"name"`
	Remote  string `json:"remote,omitempty"`
	Current bool   `json:"current,omitempty"`
}

type GitBranches struct {
	Branches []GitBranch `json:"branches"`
	Warning  string      `json:"warning,omitempty"`
}

type GitCommit struct {
	Hash       string   `json:"hash"`
	Parents    []string `json:"parents"`
	Summary    string   `json:"summary"`
	Author     string   `json:"author"`
	AuthoredAt string   `json:"authored_at"`
	Refs       []string `json:"refs"`
}

const gitRequestLimit = 1 << 20

func decodeGitParams(data json.RawMessage, target any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		data = []byte("{}")
	}
	if len(data) > gitRequestLimit || data[0] != '{' {
		return ahpInvalid("params must be an object of at most 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ahpInvalid("invalid params: " + err.Error())
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ahpInvalid("expected a single params object")
	}
	return nil
}

func (s *Server) gitStatus(ctx context.Context) (GitStatus, error) {
	status, err := s.workspace.GitStatus(ctx)
	if err != nil {
		return GitStatus{}, err
	}
	result := GitStatus{Branch: status.Branch, Head: status.Head, Upstream: status.Upstream,
		Ahead: status.Ahead, Behind: status.Behind, HasRemote: status.HasRemote, Files: []GitFileStatus{}}
	for _, f := range status.Files {
		result.Files = append(result.Files, GitFileStatus{Path: f.Path, OriginalPath: f.OriginalPath,
			IndexStatus: f.IndexStatus, WorktreeStatus: f.WorktreeStatus, Staged: f.Staged, Changed: f.Changed, Conflict: f.Conflict})
	}
	return result, nil
}

func (s *Server) gitBranches(ctx context.Context, refresh bool) (GitBranches, error) {
	if refresh {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	branches, warning, err := s.workspace.GitBranches(ctx, refresh)
	if err != nil {
		return GitBranches{}, err
	}
	result := GitBranches{Branches: []GitBranch{}, Warning: warning}
	for _, b := range branches {
		result.Branches = append(result.Branches, GitBranch{Name: b.Name, Remote: b.Remote, Current: b.Current})
	}
	return result, nil
}

func (s *Server) gitHistory(ctx context.Context) ([]GitCommit, error) {
	commits, err := s.workspace.GitHistory(ctx)
	if err != nil {
		return nil, err
	}
	result := []GitCommit{}
	for _, c := range commits {
		result = append(result, GitCommit{Hash: c.Hash, Parents: c.Parents, Summary: c.Summary,
			Author: c.Author, AuthoredAt: c.AuthoredAt.Format(time.RFC3339), Refs: c.Refs})
	}
	return result, nil
}

func (s *Server) gitInsights(ctx context.Context) (changes.GitInsightsResult, error) {
	result, err := s.workspace.GitInsights(ctx)
	if err != nil {
		return changes.GitInsightsResult{}, err
	}
	if result.Weeks == nil {
		result.Weeks = []changes.WeekActivity{}
	}
	if result.AuthorWeeks == nil {
		result.AuthorWeeks = []changes.AuthorSeries{}
	}
	if result.Authors == nil {
		result.Authors = []changes.AuthorStat{}
	}
	if result.Modules == nil {
		result.Modules = []changes.ModuleActivity{}
	}
	if result.Churn == nil {
		result.Churn = []changes.ChurnStat{}
	}
	return result, nil
}

type gitAction struct {
	Action  string   `json:"action"`
	Name    string   `json:"name,omitempty"`
	Remote  string   `json:"remote,omitempty"`
	Message string   `json:"message,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

type gitRunResult struct {
	Output string `json:"output"`
}

func (s *Server) runGit(ctx context.Context, p gitAction) (gitRunResult, error) {
	result := gitRunResult{}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var err error
	flush := false
	switch p.Action {
	case "init":
		err = s.workspace.GitInit()
		if err == nil {
			s.broadcast(Frame{Type: EvtWorkspaceChanged})
			flush = true
		}
	case "createBranch":
		p.Name = strings.TrimSpace(p.Name)
		err = s.workspace.GitCreateBranch(ctx, p.Name)
		result.Output, flush = "Created branch "+p.Name, true
	case "checkout":
		p.Name, p.Remote = strings.TrimSpace(p.Name), strings.TrimSpace(p.Remote)
		err = s.workspace.GitCheckoutBranch(ctx, p.Name, p.Remote)
		label := p.Name
		if p.Remote != "" {
			label = p.Remote + "/" + p.Name
		}
		result.Output, flush = "Switched to "+label, true
	case "stage", "unstage", "revert":
		paths := []string{}
		if len(p.Paths) == 0 {
			return result, ahpInvalid("at least one path is required")
		}
		for _, path := range p.Paths {
			rel, ok := s.workspaceRel(path)
			if !ok {
				return result, ahpInvalid("invalid path")
			}
			paths = append(paths, rel)
		}
		switch p.Action {
		case "stage":
			err = s.workspace.GitStage(ctx, paths)
		case "unstage":
			err = s.workspace.GitUnstage(ctx, paths)
		case "revert":
			if len(paths) != 1 {
				return result, ahpInvalid("revert requires one path")
			}
			err = s.workspace.RevertChange(ctx, paths[0])
			flush = true
		}
	case "commit":
		message := strings.TrimSpace(p.Message)
		if message == "" || len(message) > 10000 {
			return result, ahpInvalid("commit message must contain 1 to 10000 bytes")
		}
		result.Output, err = s.workspace.GitCommit(ctx, message)
	case "pull":
		result.Output, err = s.workspace.GitPull(ctx)
		flush = true
	case "push":
		result.Output, err = s.workspace.GitPush(ctx)
	default:
		return result, ahpInvalid("unknown Git action")
	}
	if err != nil {
		return result, err
	}
	if flush {
		s.flushFiles()
	}
	return result, nil
}

func decodeGitAction(data json.RawMessage) (gitAction, error) {
	var p gitAction
	if err := decodeGitParams(data, &p); err != nil {
		return p, err
	}
	// Reject fields unrelated to the selected operation instead of silently
	// ignoring a misspelled or misplaced argument on a repository mutation.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	allowed := " action "
	switch p.Action {
	case "init", "pull", "push":
	case "createBranch":
		allowed += "name "
	case "checkout":
		allowed += "name remote "
	case "commit":
		allowed += "message "
	default:
		return p, ahpInvalid("unknown Git action; file operations use AHP changesets")
	}
	for key := range fields {
		if !strings.Contains(allowed, " "+key+" ") {
			return p, ahpInvalid("unexpected field: " + key)
		}
	}
	return p, nil
}

func (s *Server) handleWorkspaceGit(w http.ResponseWriter, r *http.Request) {
	if !s.workspace.HasChanges() {
		writeJSON(w, nil)
		return
	}
	result, err := s.gitStatus(r.Context())
	writeGitResult(w, result, err)
}

// HTTP commands remain deliberately small. File edits and layer mutations use AHP.
func (s *Server) handleGitBranches(w http.ResponseWriter, r *http.Request) {
	result, err := s.gitBranches(r.Context(), false)
	writeGitResult(w, result, err)
}
func (s *Server) handleGitFetch(w http.ResponseWriter, r *http.Request) {
	result, err := s.gitBranches(r.Context(), true)
	writeGitResult(w, result, err)
}
func (s *Server) handleGitCommits(w http.ResponseWriter, r *http.Request) {
	result, err := s.gitHistory(r.Context())
	writeGitResult(w, result, err)
}
func (s *Server) handleWorkspaceInsights(w http.ResponseWriter, r *http.Request) {
	result, err := s.gitInsights(r.Context())
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
			writeGitResult(w, nil, ahpInvalid("action is determined by the endpoint"))
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
		result, err := s.runGit(ctx, p)
		if err == nil {
			s.ahpHost().refreshChangesets()
		}
		writeGitResult(w, result, err)
	}
}
func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	base, head, mode := strings.TrimSpace(query.Get("base")), strings.TrimSpace(query.Get("head")), query.Get("mode")
	if base == "" || head == "" {
		writeGitResult(w, nil, ahpInvalid("base and head revisions are required"))
		return
	}
	if mode == "" {
		mode = "direct"
	}
	if mode != "direct" && mode != "merge-base" {
		writeGitResult(w, nil, ahpInvalid("invalid compare mode"))
		return
	}
	comparison, err := s.workspace.GitCompare(r.Context(), base, head, mode == "merge-base")
	if err != nil {
		writeGitResult(w, nil, err)
		return
	}
	channel, err := s.ahpHost().comparisonChangeset(comparison.Diffs)
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
	var rpc *ahpRPCError
	if errors.As(err, &rpc) && rpc.Code == -32602 {
		status = http.StatusBadRequest
	}
	if errors.Is(err, changes.ErrNotGitRepository) {
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}
