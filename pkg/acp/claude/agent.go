package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/coder/acp-go-sdk"

	extclaude "github.com/adrianliechti/wingman-agent/pkg/external/claude"
)

func binPath() string {
	if path, err := extclaude.BinPath(); err == nil {
		return path
	}
	return "claude"
}

// Options configures an [Agent].
type Options struct {
	// Model is the default model id used for new sessions. Empty / "default"
	// defers to the CLI's configured default.
	Model string

	// Effort is the default reasoning effort applied to new sessions.
	// Empty / "default" disables the `--effort` flag.
	Effort string

	// Cwd is the fallback working directory used when an ACP NewSession
	// request omits one (or for the per-turn `claude` CLI spawn). The
	// per-session cwd from the ACP request still wins when present.
	Cwd string

	// Env is the environment applied to each `claude` CLI subprocess
	// spawned per turn. nil means inherit the parent process env. To layer
	// Wingman routing on top, callers can pass
	// `pkg/external/claude.BuildEnv(os.Environ(), cfg)`.
	Env []string

	// Path is the `claude` binary path. Empty resolves it from the
	// WINGMAN_CLAUDE_PATH override, then the usual PATH / installer lookup.
	Path string
}

// Agent implements [acp.Agent] for the Claude CLI. It spawns the `claude`
// CLI as a one-shot subprocess per turn (see session.runTurn); the Agent
// itself holds only in-memory session state and may be embedded directly
// into a host process — no separate ACP server binary is required.
type Agent struct {
	conn *acp.AgentSideConnection

	mu       sync.Mutex
	sessions map[acp.SessionId]*session

	defaultModel  string
	defaultEffort string
	defaultCwd    string
	env           []string
	path          string

	modelsMu     sync.Mutex
	modelsLoaded bool
	models       []ModelEntry
}

var _ acp.Agent = (*Agent)(nil)

// New constructs an Agent from opts. Pass the result to
// [acp.NewAgentSideConnection] and then call [Agent.SetAgentConnection]
// with the returned connection.
func New(opts Options) *Agent {
	model := opts.Model
	if model == "" {
		model = "default"
	}
	path := opts.Path
	if path == "" {
		path = binPath()
	}
	return &Agent{
		sessions:      make(map[acp.SessionId]*session),
		defaultModel:  model,
		defaultEffort: opts.Effort,
		defaultCwd:    opts.Cwd,
		env:           opts.Env,
		path:          path,
	}
}

func (a *Agent) SetAgentConnection(conn *acp.AgentSideConnection) { a.conn = conn }

// Close terminates every session's live process. Wired as the in-process
// cleanup so swapping away from / shutting down the backend doesn't orphan
// claude subprocesses.
func (a *Agent) Close() error {
	a.mu.Lock()
	sessions := make([]*session, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.sessions = make(map[acp.SessionId]*session)
	a.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	return nil
}

func (a *Agent) lookup(id acp.SessionId) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

// ensureModels populates the model picker from the `claude` CLI exactly once.
// Failure is non-fatal: the selector is simply omitted (empty list) and the CLI
// still runs on its configured default model.
func (a *Agent) ensureModels(ctx context.Context) {
	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	if a.modelsLoaded {
		return
	}
	models, err := fetchModels(ctx, a.path, a.env)
	if err != nil {
		return // retry on the next call rather than caching the failure
	}
	a.models = models
	a.modelsLoaded = true
}

// --- acp.Agent ---

func (a *Agent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	title := "Claude (ACP)"
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo: &acp.Implementation{
			Name:    "claude-acp",
			Title:   &title,
			Version: "0.1.0",
		},
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: acp.SessionCapabilities{
				AdditionalDirectories: &acp.SessionAdditionalDirectoriesCapabilities{},
				Close:                 &acp.SessionCloseCapabilities{},
				List:                  &acp.SessionListCapabilities{},
				Resume:                &acp.SessionResumeCapabilities{},
				Fork:                  &acp.SessionForkCapabilities{},
				Delete:                &acp.SessionDeleteCapabilities{},
			},
		},
	}, nil
}

func (a *Agent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *Agent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}

func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.ensureModels(ctx)
	id := acp.SessionId(newUUID())
	cwd := params.Cwd
	if cwd == "" {
		cwd = a.defaultCwd
	}
	s := newSession(id, cwd, a.defaultModel, a.defaultEffort, params.AdditionalDirectories)
	a.mu.Lock()
	a.sessions[id] = s
	a.mu.Unlock()

	return acp.NewSessionResponse{
		SessionId:     id,
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	s := a.lookup(params.SessionId)
	if s == nil {
		return acp.PromptResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	stop, err := s.runTurn(ctx, a.conn, a.path, a.env, params.Prompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: stop}, nil
}

func (a *Agent) Cancel(_ context.Context, params acp.CancelNotification) error {
	if s := a.lookup(params.SessionId); s != nil {
		s.cancelTurn()
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
	s := a.lookup(v.SessionId)
	if s == nil {
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("session %s not found", v.SessionId)
	}
	value := string(v.Value)

	s.mu.Lock()
	defer s.mu.Unlock()
	switch string(v.ConfigId) {
	case modelConfigID:
		m := findModel(a.models, value)
		if m == nil {
			return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown model %q", value)
		}
		s.modelID = value
		if !isValidEffort(m, s.effort) {
			s.effort = "default"
		}
	case effortConfigID:
		m := findModel(a.models, s.modelID)
		if m == nil || !isValidEffort(m, value) {
			return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("effort %q invalid for model %s", value, s.modelID)
		}
		s.effort = value
	default:
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown configId %q", v.ConfigId)
	}
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
		s.close()
	}
	return acp.CloseSessionResponse{}, nil
}

// UnstableDeleteSession tears down any live process for the session, then
// removes its on-disk transcript. Tearing down first stops the process from
// rewriting the file after deletion.
func (a *Agent) UnstableDeleteSession(_ context.Context, params acp.UnstableDeleteSessionRequest) (acp.UnstableDeleteSessionResponse, error) {
	a.mu.Lock()
	s := a.sessions[params.SessionId]
	delete(a.sessions, params.SessionId)
	a.mu.Unlock()
	if s != nil {
		s.close()
	}
	if err := deleteProjectSession(params.SessionId); err != nil {
		return acp.UnstableDeleteSessionResponse{}, err
	}
	return acp.UnstableDeleteSessionResponse{}, nil
}

func (a *Agent) ListSessions(_ context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	cwd := ""
	if params.Cwd != nil {
		cwd = *params.Cwd
	}
	sessions, err := listProjectSessions(cwd)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}
	return acp.ListSessionsResponse{Sessions: sessions}, nil
}

func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	if !sessionExists(params.Cwd, params.SessionId) {
		return acp.ResumeSessionResponse{}, fmt.Errorf("no on-disk session %s for cwd %s", params.SessionId, params.Cwd)
	}
	a.ensureModels(ctx)
	s := a.adoptSession(params.SessionId, params.Cwd, params.AdditionalDirectories, string(params.SessionId), false)
	return acp.ResumeSessionResponse{
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

func (a *Agent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	if !sessionExists(params.Cwd, params.SessionId) {
		return acp.LoadSessionResponse{}, fmt.Errorf("no on-disk session %s for cwd %s", params.SessionId, params.Cwd)
	}
	a.ensureModels(ctx)
	s := a.adoptSession(params.SessionId, params.Cwd, params.AdditionalDirectories, string(params.SessionId), false)
	if err := replayHistory(ctx, a.conn, params.SessionId, params.Cwd); err != nil {
		return acp.LoadSessionResponse{}, fmt.Errorf("replay history: %w", err)
	}
	return acp.LoadSessionResponse{
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

func (a *Agent) UnstableForkSession(_ context.Context, params acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error) {
	newID := acp.SessionId(newUUID())
	a.adoptSession(newID, params.Cwd, params.AdditionalDirectories, string(params.SessionId), true)
	// Models / ConfigOptions are intentionally omitted: the fork variant uses
	// `Unstable*` shapes that diverge from the stable selectors we build for
	// new/resume/load. Clients that need the model picker after forking can
	// query via the regular session lifecycle.
	return acp.UnstableForkSessionResponse{SessionId: newID}, nil
}

// adoptSession installs a session record in the agent map for resume / load /
// fork. The first turn's argv is determined by cliArgsLocked from the
// resumeFrom / forkOnResume fields.
func (a *Agent) adoptSession(id acp.SessionId, cwd string, additionalDirs []string, resumeFrom string, fork bool) *session {
	if cwd == "" {
		cwd = a.defaultCwd
	}
	s := newSession(id, cwd, a.defaultModel, a.defaultEffort, additionalDirs)
	s.resumeFrom = resumeFrom
	s.forkOnResume = fork
	a.mu.Lock()
	a.sessions[id] = s
	a.mu.Unlock()
	return s
}

// newUUID returns a random RFC 4122 v4 UUID. The Claude CLI's --session-id
// flag rejects anything that doesn't match this shape.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is fatal in practice; fall back to a fixed value
		// so we surface a clear error from the CLI rather than crash.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}
