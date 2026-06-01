package codex

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
)

// Agent implements [acp.Agent] for the codex app-server. It manages the
// long-lived `codex app-server` subprocess transparently (see [Spawn]) and
// exposes only ACP session-scoped operations to the host. Embed it in a
// host process via [Spawn] or [Run] — no separate ACP server binary is
// required.
type Agent struct {
	conn  *acp.AgentSideConnection
	codex *codexClient

	cmd       *exec.Cmd
	stdin     io.WriteCloser
	closeOnce sync.Once
	closed    chan struct{}

	mu       sync.Mutex
	sessions map[acp.SessionId]*session

	defaultModel  string
	defaultEffort string

	modelsOnce sync.Once
	models     []modelEntry
}

var _ acp.Agent = (*Agent)(nil)

// newAgent wires the in-memory pieces together; subprocess setup lives
// in [Spawn] so callers that supply their own codex transport (tests,
// alternate IPCs) can construct an Agent without forking.
func newAgent(codex *codexClient, model, effort string) *Agent {
	return &Agent{
		codex:         codex,
		sessions:      make(map[acp.SessionId]*session),
		closed:        make(chan struct{}),
		defaultModel:  model,
		defaultEffort: effort,
	}
}

func (a *Agent) SetAgentConnection(conn *acp.AgentSideConnection) { a.conn = conn }

func (a *Agent) lookup(id acp.SessionId) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

// ensureModels populates the model picker from codex's `model/list` exactly
// once. Failure is non-fatal: the selector is simply omitted (empty list) and
// codex still runs on its configured default.
func (a *Agent) ensureModels(ctx context.Context) {
	a.modelsOnce.Do(func() {
		var all []codexModel
		var cursor *string
		for {
			resp, err := a.codex.modelList(ctx, modelListParams{Cursor: cursor})
			if err != nil {
				return
			}
			all = append(all, resp.Data...)
			if resp.NextCursor == nil || *resp.NextCursor == "" {
				break
			}
			cursor = resp.NextCursor
		}
		a.models = modelsFromCodex(all)
	})
}

// --- acp.Agent ---

func (a *Agent) Initialize(ctx context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	if err := a.codex.initialize(ctx, initializeParams{
		ClientInfo: clientInfo{Name: "codex-acp", Title: "Codex (ACP)", Version: "0.1.0"},
	}); err != nil {
		return acp.InitializeResponse{}, fmt.Errorf("codex initialize: %w", err)
	}
	a.ensureModels(ctx)

	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo: &acp.Implementation{
			Name:    "codex-acp",
			Title:   acp.Ptr("Codex (ACP)"),
			Version: "0.1.0",
		},
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: true,
			PromptCapabilities: acp.PromptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
			SessionCapabilities: acp.SessionCapabilities{
				Close:  &acp.SessionCloseCapabilities{},
				List:   &acp.SessionListCapabilities{},
				Resume: &acp.SessionResumeCapabilities{},
			},
		},
	}, nil
}

func (a *Agent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	startParams := threadStartParams{Cwd: params.Cwd, Config: trustedCwdConfig(params.Cwd)}
	if a.defaultModel != "" && a.defaultModel != "default" {
		startParams.Model = a.defaultModel
	}

	resp, err := a.codex.threadStart(ctx, startParams)
	if err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("thread/start: %w", err)
	}
	if resp.Thread.ID == "" {
		return acp.NewSessionResponse{}, fmt.Errorf("codex returned empty thread id")
	}

	s := a.registerSession(acp.SessionId(resp.Thread.ID), resp.Model, derefEffort(resp.ReasoningEffort))
	return acp.NewSessionResponse{
		SessionId:     s.id,
		Models:        buildSessionModelState(a.models, s.modelID),
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

// registerSession installs a session record under the given id, falling back to
// the agent-level defaults when codex didn't supply a model/effort.
func (a *Agent) registerSession(id acp.SessionId, model, effort string) *session {
	if model == "" {
		model = a.defaultModel
	}
	if effort == "" {
		effort = a.defaultEffort
	}
	s := newSession(id, model, effort)
	a.mu.Lock()
	a.sessions[id] = s
	a.mu.Unlock()
	return s
}

func derefEffort(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func trustedCwdConfig(cwd string) map[string]any {
	if cwd == "" {
		return nil
	}
	return map[string]any{
		"projects": map[string]any{
			cwd: map[string]any{"trust_level": "trusted"},
		},
	}
}

func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	s := a.lookup(params.SessionId)
	if s == nil {
		return acp.PromptResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	stop, err := s.runTurn(ctx, a.conn, a.codex, params.Prompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: stop}, nil
}

func (a *Agent) Cancel(ctx context.Context, params acp.CancelNotification) error {
	if s := a.lookup(params.SessionId); s != nil {
		s.interrupt(ctx, a.codex)
	}
	return nil
}

func (a *Agent) SetSessionMode(_ context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	s := a.lookup(params.SessionId)
	if s == nil {
		return acp.SetSessionModeResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	id := string(params.ModeId)
	if findMode(id) == nil {
		return acp.SetSessionModeResponse{}, fmt.Errorf("unknown mode %q", id)
	}
	s.mu.Lock()
	s.mode = id
	s.mu.Unlock()
	return acp.SetSessionModeResponse{}, nil
}

func (a *Agent) SetSessionConfigOption(_ context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if params.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("only value-id config options supported")
	}
	v := params.ValueId
	if string(v.ConfigId) != "effort" {
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown configId %q", v.ConfigId)
	}
	s := a.lookup(v.SessionId)
	if s == nil {
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("session %s not found", v.SessionId)
	}
	level := string(v.Value)

	s.mu.Lock()
	defer s.mu.Unlock()
	m := findModel(a.models, s.modelID)
	if m == nil || !isValidEffort(m, level) {
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("effort %q invalid for model %s", level, s.modelID)
	}
	s.effort = level
	return acp.SetSessionConfigOptionResponse{
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

func (a *Agent) CloseSession(_ context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	a.mu.Lock()
	s := a.sessions[params.SessionId]
	delete(a.sessions, params.SessionId)
	a.mu.Unlock()
	if s != nil {
		s.interrupt(context.Background(), a.codex)
	}
	return acp.CloseSessionResponse{}, nil
}

func (a *Agent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	lp := threadListParams{
		Cursor: params.Cursor,
		SourceKinds: []string{
			"cli", "vscode", "exec", "appServer",
			"subAgent", "subAgentReview", "subAgentCompact",
			"subAgentThreadSpawn", "subAgentOther", "unknown",
		},
	}
	if params.Cwd != nil && *params.Cwd != "" {
		lp.Cwd = *params.Cwd
	}
	resp, err := a.codex.threadList(ctx, lp)
	if err != nil {
		return acp.ListSessionsResponse{}, fmt.Errorf("thread/list: %w", err)
	}

	sessions := make([]acp.SessionInfo, 0, len(resp.Data))
	for _, t := range resp.Data {
		sessions = append(sessions, acp.SessionInfo{
			SessionId: acp.SessionId(t.ID),
			Cwd:       t.Cwd,
			Title:     sessionTitle(t),
			UpdatedAt: sessionUpdatedAt(t.UpdatedAt),
		})
	}
	return acp.ListSessionsResponse{Sessions: sessions, NextCursor: resp.NextCursor}, nil
}

func sessionTitle(t threadSummary) *string {
	if t.Name != nil && *t.Name != "" {
		return t.Name
	}
	if t.Preview != "" {
		p := t.Preview
		return &p
	}
	return nil
}

func sessionUpdatedAt(unix int64) *string {
	if unix == 0 {
		return nil
	}
	s := time.Unix(unix, 0).UTC().Format(time.RFC3339)
	return &s
}

func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	resp, err := a.codex.threadResume(ctx, threadResumeParams{
		ThreadID:     string(params.SessionId),
		Cwd:          params.Cwd,
		Config:       trustedCwdConfig(params.Cwd),
		ExcludeTurns: true,
	})
	if err != nil {
		return acp.ResumeSessionResponse{}, fmt.Errorf("thread/resume: %w", err)
	}
	s := a.registerSession(params.SessionId, resp.Model, derefEffort(resp.ReasoningEffort))
	return acp.ResumeSessionResponse{
		Models:        buildSessionModelState(a.models, s.modelID),
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

func (a *Agent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	resp, err := a.codex.threadResume(ctx, threadResumeParams{
		ThreadID: string(params.SessionId),
		Cwd:      params.Cwd,
		Config:   trustedCwdConfig(params.Cwd),
	})
	if err != nil {
		return acp.LoadSessionResponse{}, fmt.Errorf("thread/resume: %w", err)
	}
	s := a.registerSession(params.SessionId, resp.Model, derefEffort(resp.ReasoningEffort))
	// thread/resume omits command output (aggregatedOutput=null); recover it
	// from codex's on-disk rollout log so replayed commands show their output.
	outputs := rolloutCommandOutputs(string(params.SessionId))
	streamThreadHistory(ctx, a.conn, s.id, resp.Thread.Turns, outputs)
	return acp.LoadSessionResponse{
		Models:        buildSessionModelState(a.models, s.modelID),
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

// UnstableSetSessionModel is picked up by the SDK via interface type assertion.

func (a *Agent) UnstableSetSessionModel(ctx context.Context, params acp.UnstableSetSessionModelRequest) (acp.UnstableSetSessionModelResponse, error) {
	s := a.lookup(params.SessionId)
	if s == nil {
		return acp.UnstableSetSessionModelResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	id := string(params.ModelId)
	m := findModel(a.models, id)
	if m == nil {
		return acp.UnstableSetSessionModelResponse{}, fmt.Errorf("unknown model %q", id)
	}

	s.mu.Lock()
	s.modelID = id
	if !isValidEffort(m, s.effort) {
		s.effort = "default"
	}
	opts := buildConfigOptions(a.models, s.modelID, s.effort)
	s.mu.Unlock()

	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: params.SessionId,
		Update: acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
			SessionUpdate: "config_option_update",
			ConfigOptions: opts,
		}},
	})
	return acp.UnstableSetSessionModelResponse{}, nil
}
