// Package server exposes wingman as an ACP server. External clients
// (Zed, other IDEs) speak the ACP protocol over the subprocess stdio;
// this adapter translates each ACP RPC into a call on a
// [*coder.Agent].
//
// One coder.Agent is held per cwd (workspace) and refcounted by
// session. Inside a workspace, all sessions share model + effort —
// wingman's design — even though ACP clients expect per-session
// selection. The wider design picks "shared" over per-session because
// the wingman UX has always been agent-wide.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	coder "github.com/adrianliechti/wingman-agent/pkg/code/agent"
)

func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	cfg, err := agent.DefaultConfig()
	if err != nil {
		return err
	}
	s := &Server{
		config:     cfg,
		sessions:   map[acpsdk.SessionId]*sessionEntry{},
		workspaces: map[string]*workspaceEntry{},
	}
	s.conn = acpsdk.NewAgentSideConnection(s, out, in)
	s.conn.SetLogger(slog.Default())

	select {
	case <-s.conn.Done():
	case <-ctx.Done():
	}

	s.mu.Lock()
	sessionIDs := slices.Collect(maps.Keys(s.sessions))
	s.mu.Unlock()
	for _, id := range sessionIDs {
		_, _ = s.CloseSession(context.Background(), acpsdk.CloseSessionRequest{SessionId: id})
	}
	return nil
}

type Server struct {
	conn   *acpsdk.AgentSideConnection
	config *agent.Config

	mu         sync.Mutex
	sessions   map[acpsdk.SessionId]*sessionEntry
	workspaces map[string]*workspaceEntry
}

type sessionEntry struct {
	id        acpsdk.SessionId
	agent     *coder.Agent
	workspace *workspaceEntry
	cancel    context.CancelFunc
	closing   bool
}

type workspaceEntry struct {
	ws      *code.Workspace
	agent   *coder.Agent
	key     string
	refs    int
	initted bool // WarmUp / InitMCP run lazily once per workspace
}

func (s *Server) Initialize(_ context.Context, _ acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	return acpsdk.InitializeResponse{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: acpsdk.SessionCapabilities{
				List:   &acpsdk.SessionListCapabilities{},
				Resume: &acpsdk.SessionResumeCapabilities{},
				Close:  &acpsdk.SessionCloseCapabilities{},
			},
		},
	}, nil
}

func (s *Server) Authenticate(_ context.Context, _ acpsdk.AuthenticateRequest) (acpsdk.AuthenticateResponse, error) {
	return acpsdk.AuthenticateResponse{}, nil
}

func (s *Server) NewSession(ctx context.Context, params acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	cwd, err := normalizeCwd(params.Cwd)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	w, err := s.acquireWorkspace(ctx, cwd)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}

	sid, err := w.agent.NewSession(ctx)
	if err != nil {
		s.releaseWorkspace(w)
		return acpsdk.NewSessionResponse{}, err
	}
	acpSid := acpsdk.SessionId(sid)
	s.registerSession(acpSid, w)

	return acpsdk.NewSessionResponse{
		SessionId:     acpSid,
		Models:        modelState(w.agent),
		ConfigOptions: sessionConfigOptions(w.agent),
	}, nil
}

// acquireWorkspace returns the (refcounted) workspaceEntry for cwd,
// lazily constructing it the first time. The workspace and its
// coder.Agent live for as long as any session refers to them.
func (s *Server) acquireWorkspace(ctx context.Context, cwd string) (*workspaceEntry, error) {
	s.mu.Lock()
	if w, ok := s.workspaces[cwd]; ok {
		w.refs++
		s.mu.Unlock()
		return w, nil
	}
	s.mu.Unlock()

	ws, err := code.NewWorkspace(cwd)
	if err != nil {
		return nil, err
	}
	wa := coder.New(ws, s.config, noopUI{})

	s.mu.Lock()
	if existing, ok := s.workspaces[cwd]; ok {
		// Concurrent acquire — discard our build, reuse the winner.
		existing.refs++
		s.mu.Unlock()
		_ = wa.Close()
		ws.Close()
		return existing, nil
	}
	w := &workspaceEntry{ws: ws, agent: wa, key: cwd, refs: 1}
	s.workspaces[cwd] = w
	s.mu.Unlock()

	// Warm-up runs outside the lock — it's slow (LSP / git probe) and
	// other acquireWorkspace calls in the meantime get the entry as-is
	// and skip the init thanks to initted.
	if !w.initted {
		ws.WarmUp()
		if err := ws.InitMCP(ctx); err != nil {
			slog.Warn("workspace mcp init failed", "cwd", cwd, "err", err)
		}
		wa.AutoSelectModel(ctx)
		w.initted = true
	}
	return w, nil
}

// Caller must not hold s.mu.
func (s *Server) releaseWorkspace(w *workspaceEntry) {
	s.mu.Lock()
	w.refs--
	if w.refs > 0 {
		s.mu.Unlock()
		return
	}
	delete(s.workspaces, w.key)
	s.mu.Unlock()
	_ = w.agent.Close()
	w.ws.Close()
}

func (s *Server) registerSession(id acpsdk.SessionId, w *workspaceEntry) {
	s.mu.Lock()
	s.sessions[id] = &sessionEntry{id: id, agent: w.agent, workspace: w}
	s.mu.Unlock()
}

func normalizeCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("cwd is required")
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd must be an absolute path (got %q)", cwd)
	}
	return filepath.Clean(cwd), nil
}

func (s *Server) UnstableSetSessionModel(ctx context.Context, params acpsdk.UnstableSetSessionModelRequest) (acpsdk.UnstableSetSessionModelResponse, error) {
	sess := s.lookupSession(params.SessionId)
	if sess == nil {
		return acpsdk.UnstableSetSessionModelResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	if err := sess.agent.SetModel(ctx, string(params.ModelId)); err != nil {
		return acpsdk.UnstableSetSessionModelResponse{}, err
	}
	return acpsdk.UnstableSetSessionModelResponse{}, nil
}

func (s *Server) CloseSession(_ context.Context, params acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	s.mu.Lock()
	sess := s.sessions[params.SessionId]
	if sess == nil || sess.closing {
		s.mu.Unlock()
		return acpsdk.CloseSessionResponse{}, nil
	}
	cancel := sess.cancel
	if cancel != nil {
		sess.closing = true
	} else {
		delete(s.sessions, params.SessionId)
	}
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.releaseWorkspace(sess.workspace)
	return acpsdk.CloseSessionResponse{}, nil
}

func (s *Server) lookupSession(id acpsdk.SessionId) *sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// retainSession registers a per-prompt cancel function on the session,
// bumps the workspace refcount so a concurrent CloseSession doesn't
// tear it down mid-prompt, and returns an unregister function.
func (s *Server) retainSession(id acpsdk.SessionId, cancel context.CancelFunc) (*sessionEntry, func(), error) {
	s.mu.Lock()
	sess := s.sessions[id]
	if sess == nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("session %s not found", id)
	}
	if sess.closing {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("session %s is closing", id)
	}
	if sess.cancel != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("session %s already has a prompt in progress", id)
	}
	sess.workspace.refs++
	sess.cancel = cancel
	s.mu.Unlock()
	return sess, func() {
		s.mu.Lock()
		if s.sessions[id] == sess {
			if sess.closing {
				delete(s.sessions, id)
			} else {
				sess.cancel = nil
			}
		}
		s.mu.Unlock()
	}, nil
}

func (s *Server) Cancel(_ context.Context, params acpsdk.CancelNotification) error {
	s.mu.Lock()
	var cancel context.CancelFunc
	if sess := s.sessions[params.SessionId]; sess != nil {
		cancel = sess.cancel
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (s *Server) Prompt(ctx context.Context, params acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sess, unregister, err := s.retainSession(params.SessionId, cancel)
	if err != nil {
		return acpsdk.PromptResponse{}, err
	}
	defer s.releaseWorkspace(sess.workspace)
	defer unregister()

	defer func() {
		if err := sess.agent.Save(string(sess.id)); err != nil {
			slog.Warn("save session failed", "session", sess.id, "err", err)
		}
	}()

	notify := func(u acpsdk.SessionUpdate) {
		_ = s.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
			SessionId: params.SessionId,
			Update:    u,
		})
	}

	for msg, err := range sess.agent.Send(ctx, string(sess.id), promptToContent(params.Prompt)) {
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
			}
			notify(acpsdk.UpdateAgentMessageText("error: " + err.Error()))
			return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
		}
		for _, c := range msg.Content {
			notifyContent(notify, agent.RoleAssistant, c)
		}
	}
	return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
}

func (s *Server) ListSessions(ctx context.Context, params acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	if params.Cwd == nil || *params.Cwd == "" {
		return acpsdk.ListSessionsResponse{Sessions: []acpsdk.SessionInfo{}}, nil
	}
	cwd, err := normalizeCwd(*params.Cwd)
	if err != nil {
		return acpsdk.ListSessionsResponse{}, err
	}
	// ListSessions is cheap (filesystem scan), so we don't need to
	// retain the workspace — peek without spawning anything new.
	s.mu.Lock()
	w := s.workspaces[cwd]
	s.mu.Unlock()
	if w == nil {
		// Workspace not yet referenced — build a transient one purely to
		// enumerate its on-disk sessions, then drop it.
		ws, err := code.NewWorkspace(cwd)
		if err != nil {
			return acpsdk.ListSessionsResponse{}, err
		}
		defer ws.Close()
		w = &workspaceEntry{ws: ws, agent: coder.New(ws, s.config, noopUI{}), key: cwd}
	}
	infos, err := w.agent.ListSessions(ctx)
	if err != nil {
		return acpsdk.ListSessionsResponse{}, err
	}
	out := make([]acpsdk.SessionInfo, 0, len(infos))
	for _, si := range infos {
		info := acpsdk.SessionInfo{
			SessionId: acpsdk.SessionId(si.ID),
			Cwd:       cwd,
		}
		if si.Title != "" {
			t := si.Title
			info.Title = &t
		}
		if !si.UpdatedAt.IsZero() {
			u := si.UpdatedAt.UTC().Format(time.RFC3339)
			info.UpdatedAt = &u
		}
		out = append(out, info)
	}
	return acpsdk.ListSessionsResponse{Sessions: out}, nil
}

func (s *Server) ResumeSession(ctx context.Context, params acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	w, _, err := s.loadAndAttach(ctx, params.Cwd, params.SessionId)
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	if w == nil {
		return acpsdk.ResumeSessionResponse{}, nil
	}
	return acpsdk.ResumeSessionResponse{
		Models:        modelState(w.agent),
		ConfigOptions: sessionConfigOptions(w.agent),
	}, nil
}

func (s *Server) LoadSession(ctx context.Context, params acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	w, messages, err := s.loadAndAttach(ctx, params.Cwd, params.SessionId)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	if w == nil {
		return acpsdk.LoadSessionResponse{}, nil
	}
	s.replayMessages(ctx, params.SessionId, messages)
	return acpsdk.LoadSessionResponse{
		Models:        modelState(w.agent),
		ConfigOptions: sessionConfigOptions(w.agent),
	}, nil
}

// loadAndAttach acquires the workspace and loads the session by id. When the
// on-disk session file is missing (e.g. stale id cached in an ACP client
// from a previous run), it returns (nil, nil, nil) so callers can respond
// with an empty session payload instead of a noisy internal error.
func (s *Server) loadAndAttach(ctx context.Context, cwdParam string, id acpsdk.SessionId) (*workspaceEntry, []agent.Message, error) {
	cwd, err := normalizeCwd(cwdParam)
	if err != nil {
		return nil, nil, err
	}
	w, err := s.acquireWorkspace(ctx, cwd)
	if err != nil {
		return nil, nil, err
	}
	if err := w.agent.LoadSession(ctx, string(id)); err != nil {
		s.releaseWorkspace(w)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("session %s not found: %w", id, err)
	}
	s.registerSession(id, w)
	return w, w.agent.Messages(string(id)), nil
}

func (s *Server) replayMessages(ctx context.Context, sid acpsdk.SessionId, messages []agent.Message) {
	notify := func(u acpsdk.SessionUpdate) {
		_ = s.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
			SessionId: sid,
			Update:    u,
		})
	}
	for _, m := range messages {
		if m.Hidden {
			continue
		}
		for _, c := range m.Content {
			notifyContent(notify, m.Role, c)
		}
	}
}

func notifyContent(notify func(acpsdk.SessionUpdate), role agent.MessageRole, c agent.Content) {
	switch {
	case c.ToolCall != nil:
		raw := parseRawInput(c.ToolCall.Args)
		opts := []acpsdk.ToolCallStartOpt{
			acpsdk.WithStartKind(mapKind(c.ToolCall.Name)),
			acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress),
			acpsdk.WithStartRawInput(raw),
		}
		if locs := toolLocations(c.ToolCall.Name, raw); len(locs) > 0 {
			opts = append(opts, acpsdk.WithStartLocations(locs))
		}
		notify(acpsdk.StartToolCall(
			acpsdk.ToolCallId(c.ToolCall.ID),
			toolTitle(c.ToolCall.Name, raw),
			opts...,
		))
	case c.ToolResult != nil:
		notify(acpsdk.UpdateToolCall(
			acpsdk.ToolCallId(c.ToolResult.ID),
			acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusCompleted),
			acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{
				acpsdk.ToolContent(acpsdk.TextBlock(c.ToolResult.Content)),
			}),
		))
	case c.Reasoning != nil && c.Reasoning.Summary != "":
		notify(acpsdk.UpdateAgentThoughtText(c.Reasoning.Summary))
	case c.Text != "":
		if role == agent.RoleUser {
			notify(acpsdk.UpdateUserMessageText(c.Text))
		} else {
			notify(acpsdk.UpdateAgentMessageText(c.Text))
		}
	}
}

func (s *Server) SetSessionConfigOption(ctx context.Context, params acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	if params.ValueId == nil {
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("expected select value")
	}
	p := params.ValueId
	sess := s.lookupSession(p.SessionId)
	if sess == nil {
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("session %s not found", p.SessionId)
	}
	switch string(p.ConfigId) {
	case "model":
		if err := sess.agent.SetModel(ctx, string(p.Value)); err != nil {
			return acpsdk.SetSessionConfigOptionResponse{}, err
		}
	case "effort":
		if err := sess.agent.SetEffort(ctx, string(p.Value)); err != nil {
			return acpsdk.SetSessionConfigOptionResponse{}, err
		}
	default:
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown config id: %s", p.ConfigId)
	}
	// Strict clients (Zed) treat a missing/null configOptions as "session
	// has no options" and wipe the entire picker UI — return the full list.
	return acpsdk.SetSessionConfigOptionResponse{
		ConfigOptions: sessionConfigOptions(sess.agent),
	}, nil
}

func (s *Server) SetSessionMode(_ context.Context, _ acpsdk.SetSessionModeRequest) (acpsdk.SetSessionModeResponse, error) {
	return acpsdk.SetSessionModeResponse{}, nil
}

type noopUI struct{}

func (noopUI) Ask(_ context.Context, _ string) (string, error) { return "", nil }
func (noopUI) Confirm(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func promptToContent(blocks []acpsdk.ContentBlock) []agent.Content {
	out := make([]agent.Content, 0, len(blocks))
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			out = append(out, agent.Content{Text: b.Text.Text})
		case b.ResourceLink != nil:
			out = append(out, agent.Content{Text: fmt.Sprintf("[Resource: %s]", b.ResourceLink.Uri)})
		}
	}
	return out
}

func parseRawInput(args string) any {
	if args == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		return v
	}
	return args
}

// modelState returns the [acpsdk.SessionModelState] derived from the
// agent's currently visible catalog. Returns nil when the agent has no
// models (the spec allows missing Models).
func modelState(a *coder.Agent) *acpsdk.SessionModelState {
	available, current := a.Models()
	if len(available) == 0 {
		return nil
	}
	models := make([]acpsdk.ModelInfo, 0, len(available))
	for _, m := range available {
		models = append(models, acpsdk.ModelInfo{
			ModelId: acpsdk.ModelId(m.ID),
			Name:    m.Name,
		})
	}
	if current == "" {
		current = string(models[0].ModelId)
	}
	return &acpsdk.SessionModelState{
		AvailableModels: models,
		CurrentModelId:  acpsdk.ModelId(current),
	}
}

// sessionConfigOptions composes the "model" + "effort" select options
// from the agent's catalog. Strict clients (Zed) treat a non-empty
// config_options as canonical, so both selectors must live here.
func sessionConfigOptions(a *coder.Agent) []acpsdk.SessionConfigOption {
	return []acpsdk.SessionConfigOption{
		modelConfigOption(a),
		effortConfigOption(a),
	}
}

func modelConfigOption(a *coder.Agent) acpsdk.SessionConfigOption {
	available, current := a.Models()
	opts := make(acpsdk.SessionConfigSelectOptionsUngrouped, 0, len(available))
	for _, m := range available {
		opts = append(opts, acpsdk.SessionConfigSelectOption{
			Value: acpsdk.SessionConfigValueId(m.ID),
			Name:  m.Name,
		})
	}
	return acpsdk.SessionConfigOption{
		Select: &acpsdk.SessionConfigOptionSelect{
			Id:           "model",
			Name:         "Model",
			CurrentValue: acpsdk.SessionConfigValueId(current),
			Options:      acpsdk.SessionConfigSelectOptions{Ungrouped: &opts},
		},
	}
}

func effortConfigOption(a *coder.Agent) acpsdk.SessionConfigOption {
	current, values := a.Effort()
	opts := make(acpsdk.SessionConfigSelectOptionsUngrouped, 0, len(values))
	for _, v := range values {
		opts = append(opts, acpsdk.SessionConfigSelectOption{
			Value: acpsdk.SessionConfigValueId(v),
			Name:  titleCase(v),
		})
	}
	return acpsdk.SessionConfigOption{
		Select: &acpsdk.SessionConfigOptionSelect{
			Id:           "effort",
			Name:         "Effort",
			CurrentValue: acpsdk.SessionConfigValueId(current),
			Options:      acpsdk.SessionConfigSelectOptions{Ungrouped: &opts},
		},
	}
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ─── tool-call shape helpers (unchanged from the previous version) ──

// toolTitle returns a human-readable title for a tool call.
func toolTitle(name string, raw any) string {
	args, _ := raw.(map[string]any)
	str := func(key string) string {
		if args == nil {
			return ""
		}
		v, _ := args[key].(string)
		return v
	}
	var detail string
	switch name {
	case "read", "write", "edit", "ls":
		detail = str("path")
	case "find":
		if p := str("pattern"); p != "" {
			detail = p
			if dir := str("path"); dir != "" {
				detail = p + " in " + dir
			}
		}
	case "grep":
		if p := str("pattern"); p != "" {
			detail = p
			if path := str("path"); path != "" {
				detail = p + " in " + path
			}
		}
	case "shell":
		if cmd := str("command"); cmd != "" {
			if desc := str("description"); desc != "" {
				detail = desc
			} else {
				detail = cmd
				if len(detail) > 60 {
					detail = detail[:57] + "..."
				}
			}
		}
	case "web_fetch":
		detail = str("url")
	case "web_search":
		detail = str("query")
	case "agent":
		if p := str("prompt"); p != "" {
			detail = p
			if len(detail) > 60 {
				detail = detail[:57] + "..."
			}
		}
	}
	if detail == "" {
		return name
	}
	return name + ": " + strings.TrimSpace(detail)
}

func toolLocations(name string, raw any) []acpsdk.ToolCallLocation {
	args, _ := raw.(map[string]any)
	if args == nil {
		return nil
	}
	path, _ := args["path"].(string)
	switch name {
	case "read", "write", "edit", "ls", "find", "grep":
		if path != "" {
			return []acpsdk.ToolCallLocation{{Path: path}}
		}
	}
	return nil
}

func mapKind(name string) acpsdk.ToolKind {
	switch name {
	case "read", "ls", "find":
		return acpsdk.ToolKindRead
	case "grep", "web_search":
		return acpsdk.ToolKindSearch
	case "web_fetch":
		return acpsdk.ToolKindFetch
	case "write", "edit":
		return acpsdk.ToolKindEdit
	case "shell":
		return acpsdk.ToolKindExecute
	default:
		return acpsdk.ToolKindOther
	}
}
