package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
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
	for id, sess := range s.sessions {
		delete(s.sessions, id)
		sess.workspace.refs = 0
	}
	for key, w := range s.workspaces {
		w.ws.Close()
		delete(s.workspaces, key)
	}
	s.mu.Unlock()

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
	sid := acp.SessionId(uuid.NewString())
	_, models, opts, err := s.attachSession(ctx, params.Cwd, sid)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	return acp.NewSessionResponse{
		SessionId:     sid,
		Models:        models,
		ConfigOptions: opts,
	}, nil
}

// On error after acquireWorkspace, caller must releaseWorkspace to keep refs honest.
func (s *Server) attachSession(ctx context.Context, cwd string, id acp.SessionId) (*code.Agent, *acp.SessionModelState, []acp.SessionConfigOption, error) {
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
	key, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	key = filepath.Clean(key)

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
		s.mu.Unlock()
		ws.Close()
		s.mu.Lock()
		existing.refs++
		s.mu.Unlock()
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
	delete(s.sessions, params.SessionId)
	s.mu.Unlock()

	if sess != nil {
		s.releaseWorkspace(sess.workspace)
	}
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

func (s *Server) Cancel(_ context.Context, _ acp.CancelNotification) error { return nil }

func (s *Server) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	sess := s.lookupSession(params.SessionId)
	if sess == nil {
		return acp.PromptResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
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
			switch {
			case c.ToolCall != nil:
				notify(acp.StartToolCall(
					acp.ToolCallId(c.ToolCall.ID),
					c.ToolCall.Name,
					acp.WithStartKind(mapKind(c.ToolCall.Name)),
					acp.WithStartStatus(acp.ToolCallStatusInProgress),
					acp.WithStartRawInput(parseRawInput(c.ToolCall.Args)),
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
				notify(acp.UpdateAgentMessageText(c.Text))
			}
		}
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (s *Server) ListSessions(_ context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	if params.Cwd == nil || *params.Cwd == "" {
		return acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}, nil
	}
	cwd := *params.Cwd

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
	saved, err := session.Load(code.SessionsDir(params.Cwd), string(params.SessionId))
	if err != nil {
		return acp.ResumeSessionResponse{}, fmt.Errorf("session %s not found: %w", params.SessionId, err)
	}

	a, models, opts, err := s.attachSession(ctx, params.Cwd, params.SessionId)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	a.Messages = saved.State.Messages
	a.Usage = saved.State.Usage

	return acp.ResumeSessionResponse{
		Models:        models,
		ConfigOptions: opts,
	}, nil
}

func (s *Server) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	saved, err := session.Load(code.SessionsDir(params.Cwd), string(params.SessionId))
	if err != nil {
		return acp.LoadSessionResponse{}, fmt.Errorf("session %s not found: %w", params.SessionId, err)
	}

	a, models, opts, err := s.attachSession(ctx, params.Cwd, params.SessionId)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	a.Messages = saved.State.Messages
	a.Usage = saved.State.Usage

	s.replayMessages(ctx, params.SessionId, saved.State.Messages)

	return acp.LoadSessionResponse{
		Models:        models,
		ConfigOptions: opts,
	}, nil
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
			switch {
			case c.ToolCall != nil:
				notify(acp.StartToolCall(
					acp.ToolCallId(c.ToolCall.ID),
					c.ToolCall.Name,
					acp.WithStartKind(mapKind(c.ToolCall.Name)),
					acp.WithStartStatus(acp.ToolCallStatusInProgress),
					acp.WithStartRawInput(parseRawInput(c.ToolCall.Args)),
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
				if m.Role == agent.RoleUser {
					notify(acp.UpdateUserMessageText(c.Text))
				} else {
					notify(acp.UpdateAgentMessageText(c.Text))
				}
			}
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
