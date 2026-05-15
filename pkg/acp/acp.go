package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	cfg, err := agent.DefaultConfig()
	if err != nil {
		return err
	}

	s := &Server{
		config:     cfg,
		sessions:   map[acp.SessionId]*sessionEntry{},
		workspaces: map[string]*workspaceEntry{},
	}
	s.conn = acp.NewAgentSideConnection(s, out, in)
	s.conn.SetLogger(slog.Default())

	select {
	case <-s.conn.Done():
	case <-ctx.Done():
	}

	s.mu.Lock()
	sessionIDs := make([]acp.SessionId, 0, len(s.sessions))
	for id := range s.sessions {
		sessionIDs = append(sessionIDs, id)
	}
	s.mu.Unlock()

	for _, id := range sessionIDs {
		_, _ = s.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: id})
	}

	return nil
}

type Server struct {
	conn *acp.AgentSideConnection

	config *agent.Config

	mu         sync.Mutex
	sessions   map[acp.SessionId]*sessionEntry
	workspaces map[string]*workspaceEntry
}

type sessionEntry struct {
	id        acp.SessionId
	agent     *code.Agent
	workspace *workspaceEntry
	cancel    context.CancelFunc
	closing   bool
}

type workspaceEntry struct {
	ws   *code.Workspace
	key  string
	refs int
}

func (s *Server) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: acp.SessionCapabilities{
				List:   &acp.SessionListCapabilities{},
				Resume: &acp.SessionResumeCapabilities{},
				Close:  &acp.SessionCloseCapabilities{},
			},
		},
	}, nil
}

func (s *Server) Authenticate(_ context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (s *Server) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	cwd, err := normalizeCwd(params.Cwd)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	sid := acp.SessionId(uuid.NewString())
	_, models, opts, err := s.attachSession(ctx, cwd, sid)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	return acp.NewSessionResponse{
		SessionId:     sid,
		Models:        models,
		ConfigOptions: opts,
	}, nil
}

func (s *Server) attachSession(ctx context.Context, cwd string, id acp.SessionId) (*code.Agent, *acp.SessionModelState, []acp.SessionConfigOption, error) {
	if s.lookupSession(id) != nil {
		return nil, nil, nil, fmt.Errorf("session %s is already active", id)
	}

	w, err := s.acquireWorkspace(cwd)
	if err != nil {
		return nil, nil, nil, err
	}

	w.ws.WarmUp()
	if err := w.ws.InitMCP(ctx); err != nil {
		s.releaseWorkspace(w)
		return nil, nil, nil, err
	}

	a := w.ws.NewAgent(s.config, noopUI{})

	s.mu.Lock()
	if s.sessions[id] != nil {
		s.mu.Unlock()
		s.releaseWorkspace(w)
		return nil, nil, nil, fmt.Errorf("session %s is already active", id)
	}
	s.sessions[id] = &sessionEntry{id: id, agent: a, workspace: w}
	s.mu.Unlock()

	var models *acp.SessionModelState
	if available := availableModels(); len(available) > 0 {
		current := string(available[0].ModelId)
		setAgentModel(a, current)
		models = &acp.SessionModelState{
			AvailableModels: available,
			CurrentModelId:  acp.ModelId(current),
		}
	}
	return a, models, sessionConfigOptions(a), nil
}

func (s *Server) acquireWorkspace(cwd string) (*workspaceEntry, error) {
	key := cwd

	s.mu.Lock()
	if w, ok := s.workspaces[key]; ok {
		w.refs++
		s.mu.Unlock()
		return w, nil
	}
	s.mu.Unlock()

	ws, err := code.NewWorkspace(cwd)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if existing, ok := s.workspaces[key]; ok {
		existing.refs++
		s.mu.Unlock()
		ws.Close()
		return existing, nil
	}
	w := &workspaceEntry{ws: ws, key: key, refs: 1}
	s.workspaces[key] = w
	s.mu.Unlock()
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
	w.ws.Close()
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

func (s *Server) UnstableSetSessionModel(_ context.Context, params acp.UnstableSetSessionModelRequest) (acp.UnstableSetSessionModelResponse, error) {
	a := s.lookupAgent(params.SessionId)
	if a == nil {
		return acp.UnstableSetSessionModelResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}

	setAgentModel(a, string(params.ModelId))
	return acp.UnstableSetSessionModelResponse{}, nil
}

func (s *Server) CloseSession(_ context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	s.mu.Lock()
	sess := s.sessions[params.SessionId]
	if sess == nil || sess.closing {
		s.mu.Unlock()
		return acp.CloseSessionResponse{}, nil
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
	return acp.CloseSessionResponse{}, nil
}

func (s *Server) lookupAgent(id acp.SessionId) *code.Agent {
	if sess := s.lookupSession(id); sess != nil {
		return sess.agent
	}
	return nil
}

func (s *Server) lookupSession(id acp.SessionId) *sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *Server) retainSession(id acp.SessionId, cancel context.CancelFunc) (*sessionEntry, func(), error) {
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

func (s *Server) Cancel(_ context.Context, params acp.CancelNotification) error {
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

func (s *Server) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sess, unregister, err := s.retainSession(params.SessionId, cancel)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer s.releaseWorkspace(sess.workspace)
	defer unregister()

	a := sess.agent

	defer func() {
		state := agent.State{
			Messages: a.Messages,
			Usage:    a.Usage,
		}
		_ = session.Save(code.SessionsDir(sess.workspace.key), string(sess.id), state)
	}()

	notify := func(u acp.SessionUpdate) {
		_ = s.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update:    u,
		})
	}

	for msg, err := range a.Send(ctx, promptToContent(params.Prompt)) {
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
			}
			notify(acp.UpdateAgentMessageText("error: " + err.Error()))
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		for _, c := range msg.Content {
			notifyContent(notify, agent.RoleAssistant, c)
		}
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (s *Server) ListSessions(_ context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	if params.Cwd == nil || *params.Cwd == "" {
		return acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}, nil
	}
	cwd, err := normalizeCwd(*params.Cwd)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	saved, err := session.List(code.SessionsDir(cwd))
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	out := make([]acp.SessionInfo, 0, len(saved))
	for _, sess := range saved {
		info := acp.SessionInfo{
			SessionId: acp.SessionId(sess.ID),
			Cwd:       cwd,
		}
		if sess.Title != "" {
			t := sess.Title
			info.Title = &t
		}
		if !sess.UpdatedAt.IsZero() {
			u := sess.UpdatedAt.UTC().Format(time.RFC3339)
			info.UpdatedAt = &u
		}
		out = append(out, info)
	}
	return acp.ListSessionsResponse{Sessions: out}, nil
}

func (s *Server) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	models, opts, _, err := s.loadAndAttach(ctx, params.Cwd, params.SessionId)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	return acp.ResumeSessionResponse{
		Models:        models,
		ConfigOptions: opts,
	}, nil
}

func (s *Server) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	models, opts, messages, err := s.loadAndAttach(ctx, params.Cwd, params.SessionId)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	s.replayMessages(ctx, params.SessionId, messages)
	return acp.LoadSessionResponse{
		Models:        models,
		ConfigOptions: opts,
	}, nil
}

func (s *Server) loadAndAttach(ctx context.Context, cwdParam string, id acp.SessionId) (*acp.SessionModelState, []acp.SessionConfigOption, []agent.Message, error) {
	cwd, err := normalizeCwd(cwdParam)
	if err != nil {
		return nil, nil, nil, err
	}

	saved, err := session.Load(code.SessionsDir(cwd), string(id))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("session %s not found: %w", id, err)
	}

	a, models, opts, err := s.attachSession(ctx, cwd, id)
	if err != nil {
		return nil, nil, nil, err
	}
	a.Messages = saved.State.Messages
	a.Usage = saved.State.Usage

	return models, opts, saved.State.Messages, nil
}

func (s *Server) replayMessages(ctx context.Context, sid acp.SessionId, messages []agent.Message) {
	notify := func(u acp.SessionUpdate) {
		_ = s.conn.SessionUpdate(ctx, acp.SessionNotification{
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

func notifyContent(notify func(acp.SessionUpdate), role agent.MessageRole, c agent.Content) {
	switch {
	case c.ToolCall != nil:
		raw := parseRawInput(c.ToolCall.Args)
		opts := []acp.ToolCallStartOpt{
			acp.WithStartKind(mapKind(c.ToolCall.Name)),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
			acp.WithStartRawInput(raw),
		}
		if locs := toolLocations(c.ToolCall.Name, raw); len(locs) > 0 {
			opts = append(opts, acp.WithStartLocations(locs))
		}
		notify(acp.StartToolCall(
			acp.ToolCallId(c.ToolCall.ID),
			toolTitle(c.ToolCall.Name, raw),
			opts...,
		))
	case c.ToolResult != nil:
		notify(acp.UpdateToolCall(
			acp.ToolCallId(c.ToolResult.ID),
			acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
			acp.WithUpdateContent([]acp.ToolCallContent{
				acp.ToolContent(acp.TextBlock(c.ToolResult.Content)),
			}),
		))
	case c.Reasoning != nil && c.Reasoning.Summary != "":
		notify(acp.UpdateAgentThoughtText(c.Reasoning.Summary))
	case c.Text != "":
		if role == agent.RoleUser {
			notify(acp.UpdateUserMessageText(c.Text))
		} else {
			notify(acp.UpdateAgentMessageText(c.Text))
		}
	}
}

func (s *Server) SetSessionConfigOption(_ context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if params.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("expected select value")
	}
	p := params.ValueId

	a := s.lookupAgent(p.SessionId)
	if a == nil {
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("session %s not found", p.SessionId)
	}

	switch string(p.ConfigId) {
	case "model":
		setAgentModel(a, string(p.Value))
	case "effort":
		if err := setAgentEffort(a, string(p.Value)); err != nil {
			return acp.SetSessionConfigOptionResponse{}, err
		}
	default:
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown config id: %s", p.ConfigId)
	}
	// The spec requires the full updated option list on every response —
	// strict clients (Zed) treat a missing/null configOptions as "session
	// has no options" and wipe the entire picker UI.
	return acp.SetSessionConfigOptionResponse{
		ConfigOptions: sessionConfigOptions(a),
	}, nil
}

func (s *Server) SetSessionMode(_ context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

type noopUI struct{}

func (noopUI) Ask(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (noopUI) Confirm(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func promptToContent(blocks []acp.ContentBlock) []agent.Content {
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

func availableModels() []acp.ModelInfo {
	out := make([]acp.ModelInfo, 0, len(code.AvailableModels))
	for _, m := range code.AvailableModels {
		out = append(out, acp.ModelInfo{
			ModelId: acp.ModelId(m.ID),
			Name:    m.Name,
		})
	}
	return out
}

func setAgentModel(a *code.Agent, modelID string) {
	a.Config.Model = func() string { return modelID }
}

// Once config_options is non-empty, strict clients (Zed) treat it as
// canonical and ignore the legacy Models/Modes fields entirely. So every
// selector wingman wants visible — model included — has to live here.
func sessionConfigOptions(a *code.Agent) []acp.SessionConfigOption {
	return []acp.SessionConfigOption{
		modelConfigOption(currentModel(a)),
		effortConfigOption(currentEffort(a)),
	}
}

func currentModel(a *code.Agent) string {
	if a.Config.Model == nil {
		return ""
	}
	return a.Config.Model()
}

func modelConfigOption(current string) acp.SessionConfigOption {
	opts := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(code.AvailableModels))
	for _, m := range code.AvailableModels {
		opts = append(opts, acp.SessionConfigSelectOption{
			Value: acp.SessionConfigValueId(m.ID),
			Name:  m.Name,
		})
	}
	return acp.SessionConfigOption{
		Select: &acp.SessionConfigOptionSelect{
			Id:           "model",
			Name:         "Model",
			CurrentValue: acp.SessionConfigValueId(current),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &opts},
		},
	}
}

func currentEffort(a *code.Agent) string {
	if a.Config.Effort == nil {
		return "auto"
	}
	if v := a.Config.Effort(); v != "" {
		return v
	}
	return "auto"
}

func effortConfigOption(current string) acp.SessionConfigOption {
	opts := acp.SessionConfigSelectOptionsUngrouped{
		{Value: "auto", Name: "Auto"},
		{Value: "low", Name: "Low"},
		{Value: "medium", Name: "Medium"},
		{Value: "high", Name: "High"},
	}
	return acp.SessionConfigOption{
		Select: &acp.SessionConfigOptionSelect{
			Id:           "effort",
			Name:         "Effort",
			CurrentValue: acp.SessionConfigValueId(current),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &opts},
		},
	}
}

func setAgentEffort(a *code.Agent, effort string) error {
	switch effort {
	case "", "auto":
		a.Config.Effort = nil
	case "low", "medium", "high":
		v := effort
		a.Config.Effort = func() string { return v }
	default:
		return fmt.Errorf("effort must be auto, low, medium, or high (got %q)", effort)
	}
	return nil
}

// toolTitle returns a human-readable title for a tool call that includes the
// most informative argument, so IDEs can show e.g. "read: pkg/acp/acp.go"
// instead of just "read".
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
			// Use the human description when provided; fall back to the raw command.
			if desc := str("description"); desc != "" {
				detail = desc
			} else {
				detail = cmd
				if len(detail) > 60 {
					detail = detail[:57] + "..."
				}
			}
		}
	case "fetch":
		detail = str("url")
	case "search_online":
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

// toolLocations returns file locations for tools that operate on known paths,
// enabling "follow-along" navigation in IDEs.
func toolLocations(name string, raw any) []acp.ToolCallLocation {
	args, _ := raw.(map[string]any)
	if args == nil {
		return nil
	}

	path, _ := args["path"].(string)

	switch name {
	case "read", "write", "edit", "ls", "find", "grep":
		if path != "" {
			return []acp.ToolCallLocation{{Path: path}}
		}
	}
	return nil
}

func mapKind(name string) acp.ToolKind {
	switch name {
	case "read", "ls", "find":
		return acp.ToolKindRead
	case "grep", "search_online":
		return acp.ToolKindSearch
	case "fetch":
		return acp.ToolKindFetch
	case "write", "edit":
		return acp.ToolKindEdit
	case "shell":
		return acp.ToolKindExecute
	default:
		return acp.ToolKindOther
	}
}
