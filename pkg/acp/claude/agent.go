package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
	claudecli "github.com/adrianliechti/wingman-agent/pkg/external/claude"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

func binPath() string {
	return claudecli.BinPath()
}

type Options struct {
	Model string

	Effort string

	Cwd string
	Env []string

	Path string
	Args []string

	Stderr io.Writer
}

type Agent struct {
	conn acp.ClientConnection

	mu              sync.Mutex
	sessions        map[acpsdk.SessionId]*session
	formElicitation bool

	defaultModel  string
	defaultEffort string
	defaultCwd    string
	env           []string
	path          string
	args          []string
	stderr        io.Writer

	modelsMu     sync.Mutex
	modelsLoaded bool
	models       []ModelEntry
	commands     []acpsdk.AvailableCommand
}

var _ acpsdk.Agent = (*Agent)(nil)

func New(opts Options) *Agent {
	model := opts.Model
	if model == "" {
		model = "default"
	}
	path := opts.Path
	if path == "" {
		path = binPath()
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	args := opts.Args
	if args == nil {
		args = claudecli.BuildArgs(nil)
	}
	return &Agent{
		sessions:      make(map[acpsdk.SessionId]*session),
		defaultModel:  model,
		defaultEffort: opts.Effort,
		defaultCwd:    opts.Cwd,
		env:           opts.Env,
		path:          path,
		args:          slices.Clone(args),
		stderr:        stderr,
	}
}

func (a *Agent) SetClientConnection(conn acp.ClientConnection) { a.conn = conn }

func (a *Agent) Close() error {
	a.mu.Lock()
	sessions := make([]*session, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.sessions = make(map[acpsdk.SessionId]*session)
	a.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	return nil
}

func (a *Agent) supportsFormElicitation() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.formElicitation
}

func (a *Agent) lookup(id acpsdk.SessionId) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

func (a *Agent) ensureModels(ctx context.Context) {
	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	if a.modelsLoaded {
		return
	}
	models, commands, err := a.fetchModels(ctx)
	if err != nil {
		return
	}
	a.models = models
	a.commands = commands
	a.modelsLoaded = true
}

func (a *Agent) sendAvailableCommands(s *session) {
	if s == nil {
		return
	}
	a.modelsMu.Lock()
	cmds := a.commands
	a.modelsMu.Unlock()
	if len(cmds) == 0 {
		return
	}

	go func() {
		// Model discovery can finish just before a session is closed or replaced.
		// Do not publish its deferred command list to a stale session ID.
		if a.lookup(s.id) != s || a.conn == nil {
			return
		}
		_ = a.conn.SessionUpdate(context.Background(), acpsdk.SessionNotification{
			SessionId: s.id,
			Update: acpsdk.SessionUpdate{AvailableCommandsUpdate: &acpsdk.SessionAvailableCommandsUpdate{
				SessionUpdate:     "available_commands_update",
				AvailableCommands: cmds,
			}},
		})
	}()
}

func (a *Agent) Initialize(_ context.Context, params acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	a.mu.Lock()
	a.formElicitation = params.ClientCapabilities.Elicitation != nil && params.ClientCapabilities.Elicitation.Form != nil
	a.mu.Unlock()

	title := "Claude (ACP)"
	return acpsdk.InitializeResponse{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		AgentInfo: &acpsdk.Implementation{
			Name:    "claude-acp",
			Title:   &title,
			Version: "0.1.0",
		},
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession: true,
			McpCapabilities: acpsdk.McpCapabilities{
				Http: true,
				Sse:  true,
			},
			PromptCapabilities: acpsdk.PromptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
			SessionCapabilities: acpsdk.SessionCapabilities{
				AdditionalDirectories: &acpsdk.SessionAdditionalDirectoriesCapabilities{},
				Close:                 &acpsdk.SessionCloseCapabilities{},
				List:                  &acpsdk.SessionListCapabilities{},
				Resume:                &acpsdk.SessionResumeCapabilities{},
				Fork:                  &acpsdk.SessionForkCapabilities{},
				Delete:                &acpsdk.SessionDeleteCapabilities{},
			},
		},
	}, nil
}

func (a *Agent) Authenticate(context.Context, acpsdk.AuthenticateRequest) (acpsdk.AuthenticateResponse, error) {
	return acpsdk.AuthenticateResponse{}, nil
}

func (a *Agent) Logout(context.Context, acpsdk.LogoutRequest) (acpsdk.LogoutResponse, error) {
	return acpsdk.LogoutResponse{}, nil
}

func (a *Agent) NewSession(ctx context.Context, params acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	if err := acp.ValidateMCPServers(params.McpServers, acpsdk.McpCapabilities{Http: true, Sse: true}); err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	cwd, additional, err := acp.NormalizeSessionRoots(params.Cwd, params.AdditionalDirectories)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	a.ensureModels(ctx)
	id := acpsdk.SessionId(uuid.NewString())
	modelID, effort := normalizeSessionConfig(a.models, a.defaultModel, a.defaultEffort)
	s := a.newSession(id, cwd, modelID, effort, additional)
	s.mcpServers = params.McpServers
	s.allowBypass = a.bypassAllowed(cwd, params.Meta)
	a.storeSession(s)
	a.sendAvailableCommands(s)

	return acpsdk.NewSessionResponse{
		SessionId:     id,
		Modes:         buildSessionModeState(s.mode, s.allowBypass),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

func (a *Agent) Prompt(ctx context.Context, params acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	if params.MessageId != nil {
		ctx = harness.WithInputID(ctx, *params.MessageId)
	}
	s := a.lookup(params.SessionId)
	if s == nil {
		return acpsdk.PromptResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	if err := s.promptMu.Lock(ctx); err != nil {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
	}
	defer s.promptMu.Unlock()
	// A prompt that raced session/close ends like one the close cancelled.
	if s.isClosed() {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, UserMessageId: params.MessageId}, nil
	}
	if ctx.Err() != nil {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, UserMessageId: params.MessageId}, nil
	}
	stop, usage, err := s.runTurn(ctx, params.Prompt)
	if err != nil {
		return acpsdk.PromptResponse{}, err
	}
	return acpsdk.PromptResponse{StopReason: stop, Usage: usage, UserMessageId: params.MessageId}, nil
}

func (a *Agent) Cancel(_ context.Context, params acpsdk.CancelNotification) error {
	if s := a.lookup(params.SessionId); s != nil {
		s.cancelTurn()
	}
	return nil
}

func (a *Agent) SetSessionMode(_ context.Context, params acpsdk.SetSessionModeRequest) (acpsdk.SetSessionModeResponse, error) {
	s := a.lookup(params.SessionId)
	if s == nil {
		return acpsdk.SetSessionModeResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	id := string(params.ModeId)
	if findMode(id) == nil {
		return acpsdk.SetSessionModeResponse{}, fmt.Errorf("unknown mode %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == bypassModeID && !s.allowBypass {
		return acpsdk.SetSessionModeResponse{}, fmt.Errorf("mode %q is not available in this session", id)
	}
	s.setModeLocked(id)
	return acpsdk.SetSessionModeResponse{}, nil
}

func (a *Agent) SetSessionConfigOption(ctx context.Context, params acpsdk.SetSessionConfigOptionRequest) (response acpsdk.SetSessionConfigOptionResponse, err error) {
	if params.ValueId == nil {
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("only value-id config options supported")
	}
	v := params.ValueId
	s := a.lookup(v.SessionId)
	if s == nil {
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("session %s not found", v.SessionId)
	}
	value := string(v.Value)

	s.mu.Lock()
	before := buildConfigOptions(a.models, s.modelID, s.effort)
	defer func() {
		s.mu.Unlock()
		if err == nil && a.lookup(s.id) == s {
			err = acp.NotifyConfig(ctx, a.conn, s.id, before, response.ConfigOptions)
		}
	}()
	switch string(v.ConfigId) {
	case modelConfigID:
		m := resolveModel(a.models, value)
		if m == nil && value != s.modelID {
			return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown model %q", value)
		}
		if m != nil {
			s.modelID = m.ID
		}
		s.modelOverride = true
		if m != nil && !acp.IsValidEffort(m.EffortLevels, s.effort) {
			s.effort = "default"
		}
	case effortConfigID:
		m := findModel(a.models, s.modelID)
		if m == nil || !acp.IsValidEffort(m.EffortLevels, value) {
			return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("effort %q invalid for model %s", value, s.modelID)
		}
		s.effort = value
	default:
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown configId %q", v.ConfigId)
	}
	return acpsdk.SetSessionConfigOptionResponse{
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

func (a *Agent) CloseSession(_ context.Context, params acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	a.mu.Lock()
	s := a.sessions[params.SessionId]
	delete(a.sessions, params.SessionId)
	a.mu.Unlock()
	if s != nil {
		s.close()
	}
	return acpsdk.CloseSessionResponse{}, nil
}

func (a *Agent) UnstableDeleteSession(_ context.Context, params acpsdk.UnstableDeleteSessionRequest) (acpsdk.UnstableDeleteSessionResponse, error) {
	a.mu.Lock()
	s := a.sessions[params.SessionId]
	delete(a.sessions, params.SessionId)
	a.mu.Unlock()
	if s != nil {
		s.close()
	}
	if err := deleteProjectSession(params.SessionId, a.env); err != nil {
		return acpsdk.UnstableDeleteSessionResponse{}, err
	}
	return acpsdk.UnstableDeleteSessionResponse{}, nil
}

func (a *Agent) ListSessions(_ context.Context, params acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	cwd := ""
	if params.Cwd != nil {
		cwd = *params.Cwd
	}
	sessions, err := listProjectSessions(cwd, a.env)
	if err != nil {
		return acpsdk.ListSessionsResponse{}, err
	}
	return acpsdk.ListSessionsResponse{Sessions: sessions}, nil
}

func (a *Agent) ResumeSession(ctx context.Context, params acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	if err := acp.ValidateMCPServers(params.McpServers, acpsdk.McpCapabilities{Http: true, Sse: true}); err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	cwd, additional, err := acp.NormalizeSessionRoots(params.Cwd, params.AdditionalDirectories)
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	src, err := a.resumeSourceFor(cwd, params.SessionId)
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	a.ensureModels(ctx)
	s := a.adoptSession(params.SessionId, cwd, additional, params.McpServers, params.Meta, src)
	a.sendAvailableCommands(s)
	return acpsdk.ResumeSessionResponse{
		Modes:         buildSessionModeState(s.mode, s.allowBypass),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

func (a *Agent) LoadSession(ctx context.Context, params acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	if err := acp.ValidateMCPServers(params.McpServers, acpsdk.McpCapabilities{Http: true, Sse: true}); err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	cwd, additional, err := acp.NormalizeSessionRoots(params.Cwd, params.AdditionalDirectories)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	src, err := a.resumeSourceFor(cwd, params.SessionId)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	a.ensureModels(ctx)
	s := a.adoptSession(params.SessionId, cwd, additional, params.McpServers, params.Meta, src)
	if src.from != "" {
		if err := replayHistory(ctx, a.conn, params.SessionId, cwd, src.from, a.env); err != nil {
			return acpsdk.LoadSessionResponse{}, fmt.Errorf("replay history: %w", err)
		}
	}
	a.sendAvailableCommands(s)
	return acpsdk.LoadSessionResponse{
		Modes:         buildSessionModeState(s.mode, s.allowBypass),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort),
	}, nil
}

func (a *Agent) UnstableForkSession(_ context.Context, params acpsdk.UnstableForkSessionRequest) (acpsdk.UnstableForkSessionResponse, error) {
	servers, err := acp.StableMCPServers(params.McpServers, acpsdk.McpCapabilities{Http: true, Sse: true})
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	cwd, additional, err := acp.NormalizeSessionRoots(params.Cwd, params.AdditionalDirectories)
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	newID := acpsdk.SessionId(uuid.NewString())
	s := a.adoptSession(newID, cwd, additional, servers, params.Meta, resumeSource{from: string(params.SessionId), fork: true})
	a.sendAvailableCommands(s)

	return acpsdk.UnstableForkSessionResponse{
		SessionId:     newID,
		Modes:         buildSessionModeState(s.mode, s.allowBypass),
		ConfigOptions: acp.UnstableConfigOptions(buildConfigOptions(a.models, s.modelID, s.effort)),
	}, nil
}

type resumeSource struct {
	from string
	fork bool
	live *session
}

func (a *Agent) resumeSourceFor(cwd string, id acpsdk.SessionId) (resumeSource, error) {
	src := resumeSource{from: string(id)}
	if live := a.lookup(id); live != nil {
		live.mu.Lock()
		if !live.started {
			src = resumeSource{from: live.resumeFrom, fork: live.forkOnResume, live: live}
		}
		live.mu.Unlock()
	}
	// Claude Code writes a session's own transcript only at its first prompt.
	if src.from == string(id) && !sessionExists(cwd, id, a.env) {
		return resumeSource{}, fmt.Errorf("no on-disk session %s for cwd %s", id, cwd)
	}
	return src, nil
}

func (a *Agent) adoptSession(id acpsdk.SessionId, cwd string, additionalDirs []string, mcpServers []acpsdk.McpServer, meta map[string]any, src resumeSource) *session {
	modelID, effort := normalizeSessionConfig(a.models, a.defaultModel, a.defaultEffort)
	s := a.newSession(id, cwd, modelID, effort, additionalDirs)
	resumeFrom := src.from
	if resumeFrom != "" && (a.defaultModel == "" || a.defaultModel == "default") {
		if live := scanSessionModel(transcriptPath(cwd, resumeFrom, a.env)); live != "" {
			if m := resolveResumedModel(a.models, live); m != nil {
				s.modelID = m.ID
			} else {
				s.modelID = live
			}
			s.modelOverride = false
		}
	}
	s.modelID, s.effort = normalizeSessionConfig(a.models, s.modelID, s.effort)
	s.mcpServers = mcpServers
	s.allowBypass = a.bypassAllowed(cwd, meta)
	s.resumeFrom = resumeFrom
	s.forkOnResume = src.fork
	if src.live != nil {
		s.inheritConfig(src.live)
	}
	a.storeSession(s)
	return s
}

func (a *Agent) bypassAllowed(cwd string, meta map[string]any) bool {
	claudeCode, _ := meta["claudeCode"].(map[string]any)
	options, _ := claudeCode["options"].(map[string]any)
	if allow, ok := options["allowDangerouslySkipPermissions"].(bool); ok && !allow {
		return false
	}
	return !bypassDisabledBySettings(cwd, a.env)
}

func (a *Agent) storeSession(s *session) {
	a.mu.Lock()
	old := a.sessions[s.id]
	a.sessions[s.id] = s
	a.mu.Unlock()
	if old != nil && old != s {
		old.close()
	}
}

func (a *Agent) SetAgentConnection(conn *acpsdk.AgentSideConnection) { a.SetClientConnection(conn) }
