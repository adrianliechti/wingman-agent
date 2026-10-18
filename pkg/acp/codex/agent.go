package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
)

type Agent struct {
	conn  *acpsdk.AgentSideConnection
	codex *codexClient

	cmd         *exec.Cmd
	stdin       io.WriteCloser
	closeOnce   sync.Once
	closed      chan struct{}
	processErr  error
	processDone chan struct{}

	mu       sync.Mutex
	sessions map[acpsdk.SessionId]*session

	defaultModel  string
	defaultEffort string

	models []modelEntry

	clientCapabilities acpsdk.ClientCapabilities
}

var _ acpsdk.Agent = (*Agent)(nil)

func newAgent(codex *codexClient, model, effort string) *Agent {
	a := &Agent{
		codex:         codex,
		sessions:      make(map[acpsdk.SessionId]*session),
		closed:        make(chan struct{}),
		defaultModel:  model,
		defaultEffort: effort,
	}
	codex.setGlobalNotificationHandler(a.handleGlobalNotification)
	return a
}

func (a *Agent) SetAgentConnection(conn *acpsdk.AgentSideConnection) {
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
}

func (a *Agent) connection() *acpsdk.AgentSideConnection {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn
}

func (a *Agent) sessionConnection(id acpsdk.SessionId) *acpsdk.AgentSideConnection {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessions[id] == nil {
		return nil
	}
	return a.conn
}

func (a *Agent) lookup(id acpsdk.SessionId) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

func (a *Agent) handleGlobalNotification(threadID, method string, params json.RawMessage) {
	// Account-scoped: no threadId, so warn on every live session.
	if method == "account/rateLimits/updated" {
		note := rateLimitNote(params)
		a.mu.Lock()
		ids := slices.Collect(maps.Keys(a.sessions))
		conn := a.conn
		a.mu.Unlock()
		if note == "" || conn == nil || len(ids) == 0 {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, id := range ids {
				_ = notifyClient(ctx, conn, id, acpsdk.UpdateAgentMessageText(note))
			}
		}()
		return
	}
	if method != "thread/name/updated" {
		return
	}
	conn := a.sessionConnection(acpsdk.SessionId(threadID))
	if conn == nil {
		return
	}
	var p struct {
		ThreadName string `json:"threadName"`
	}
	if json.Unmarshal(params, &p) != nil || p.ThreadName == "" {
		return
	}
	go func(id acpsdk.SessionId, title string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = notifyClient(ctx, conn, id, sessionTitleUpdate(title))
	}(acpsdk.SessionId(threadID), p.ThreadName)
}

func (a *Agent) loadModels(ctx context.Context) {
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
}

// resumeModelProvider returns the currently configured provider so resumed
// threads do not silently switch to the provider persisted in their rollout.
func (a *Agent) resumeModelProvider(ctx context.Context) string {
	resp, err := a.codex.configRead(ctx, configReadParams{})
	if err != nil {
		return ""
	}
	if mp, ok := resp.Config["model_provider"].(string); ok {
		return mp
	}
	return ""
}

func (a *Agent) Initialize(ctx context.Context, req acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	a.clientCapabilities = req.ClientCapabilities
	if err := a.codex.initialize(ctx, initializeParams{
		ClientInfo:   clientInfo{Name: "codex-acp", Title: "Codex (ACP)", Version: "0.1.0"},
		Capabilities: clientCapabilities{ExperimentalAPI: true},
	}); err != nil {
		return acpsdk.InitializeResponse{}, fmt.Errorf("codex initialize: %w", err)
	}
	a.loadModels(ctx)

	return acpsdk.InitializeResponse{
		// v2 changes the prompt lifecycle, permissions, and replay. Negotiate
		// the SDK's implemented version even when the client requests a newer one.
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		AgentInfo: &acpsdk.Implementation{
			Name:    "codex-acp",
			Title:   new("Codex (ACP)"),
			Version: "0.1.0",
		},
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession: true,
			McpCapabilities: acpsdk.McpCapabilities{
				Http: true,
			},
			PromptCapabilities: acpsdk.PromptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
			SessionCapabilities: acpsdk.SessionCapabilities{
				AdditionalDirectories: &acpsdk.SessionAdditionalDirectoriesCapabilities{},
				Close:                 &acpsdk.SessionCloseCapabilities{},
				Fork:                  &acpsdk.SessionForkCapabilities{},
				List:                  &acpsdk.SessionListCapabilities{},
				Resume:                &acpsdk.SessionResumeCapabilities{},
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
	if err := acp.ValidateMCPServers(params.McpServers, acpsdk.McpCapabilities{Http: true}); err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	cwd, additional, err := acp.NormalizeSessionRoots(params.Cwd, params.AdditionalDirectories)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	startParams := threadStartParams{Cwd: cwd, Config: sessionConfig(cwd, additional, params.McpServers)}
	if a.defaultModel != "" && a.defaultModel != "default" {
		startParams.Model = a.defaultModel
	}

	mcpVersion := a.codex.currentMCPStartupVersion()
	resp, err := a.codex.threadStart(ctx, startParams)
	if err != nil {
		return acpsdk.NewSessionResponse{}, fmt.Errorf("thread/start: %w", err)
	}
	if resp.Thread.ID == "" {
		return acpsdk.NewSessionResponse{}, fmt.Errorf("codex returned empty thread id")
	}
	if err := a.awaitMCPStartup(ctx, resp.Thread.ID, params.Meta, params.McpServers, mcpVersion); err != nil {
		return acpsdk.NewSessionResponse{}, err
	}

	s := a.registerSession(acpsdk.SessionId(resp.Thread.ID), resp.Model, derefEffort(resp.ReasoningEffort), defaultCollaborationMode, additional)
	// A new thread has used no tokens yet.
	a.codex.setThreadTokenTotal(string(s.id), tokenBreakdown{})
	if err := a.sendAvailableCommands(ctx, s.id); err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	return acpsdk.NewSessionResponse{
		SessionId:     s.id,
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort, s.collaborationMode),
	}, nil
}

// awaitMCPStartup honors the experimental _meta.mcpStartupAwaitTimeoutMs: a
// positive value waits up to that long for requested MCP servers to finish
// starting. Without it, sessions are returned immediately.
func (a *Agent) awaitMCPStartup(ctx context.Context, threadID string, meta map[string]any, servers []acpsdk.McpServer, after uint64) error {
	var ms float64
	switch v := meta["mcpStartupAwaitTimeoutMs"].(type) {
	case float64:
		ms = v
	case int:
		ms = float64(v)
	}
	names := slices.Collect(maps.Keys(mcpServersConfig(servers)))
	if !(ms > 0) || len(names) == 0 {
		return nil
	}
	timeout := time.Duration(math.MaxInt64)
	if ms < float64(math.MaxInt64/int64(time.Millisecond)) {
		timeout = time.Duration(ms * float64(time.Millisecond))
	}
	if err := a.codex.awaitMCPStartup(ctx, threadID, names, after, timeout); err != nil {
		return fmt.Errorf("await MCP server startup: %w", err)
	}
	return nil
}

func (a *Agent) registerSession(id acpsdk.SessionId, model, effort, collaborationMode string, additionalDirectories []string) *session {
	if model == "" {
		model = a.defaultModel
	}
	if effort == "" {
		effort = a.defaultEffort
	}
	model, effort = normalizeSessionConfig(a.models, model, effort)
	s := newSession(id, model, effort, additionalDirectories)
	s.collaborationMode = collaborationMode
	a.mu.Lock()
	old := a.sessions[id]
	if old != nil && old != s {
		old.markClosed()
	}
	a.sessions[id] = s
	a.mu.Unlock()
	return s
}

func (a *Agent) registerResumedSession(id acpsdk.SessionId, resp threadResumeResponse, materialized bool, additionalDirectories []string) *session {
	model, effort := resp.Model, derefEffort(resp.ReasoningEffort)
	mode := resumedCollaborationMode(resp.CollaborationMode)
	if !materialized {
		// The thread/read fallback reuses the running thread, but omits
		// collaboration mode and may not reflect model/effort selections that
		// will be applied on the next turn. Preserve the local configuration.
		if old := a.lookup(id); old != nil {
			old.mu.Lock()
			model, effort = old.modelID, old.effort
			mode = old.collaborationMode
			old.mu.Unlock()
		}
	}
	return a.registerSession(id, model, effort, mode, additionalDirectories)
}

func (a *Agent) sendAvailableCommands(ctx context.Context, id acpsdk.SessionId) error {
	conn := a.connection()
	if conn == nil {
		return nil
	}
	return notifyClient(ctx, conn, id, acpsdk.SessionUpdate{AvailableCommandsUpdate: &acpsdk.SessionAvailableCommandsUpdate{
		SessionUpdate:     "available_commands_update",
		AvailableCommands: availableCommands(),
	}})
}

func availableCommands() []acpsdk.AvailableCommand {
	return []acpsdk.AvailableCommand{
		{Name: "plan", Description: "Toggle plan mode"},
		{
			Name:        "rename",
			Description: "Rename the current session.",
			Input: &acpsdk.AvailableCommandInput{Unstructured: &acpsdk.UnstructuredCommandInput{
				Hint: "new name",
			}},
		},
	}
}

func derefEffort(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func sessionConfig(cwd string, additionalDirectories []string, servers []acpsdk.McpServer) map[string]any {
	cfg := map[string]any{}
	projects := map[string]any{}
	for _, root := range append([]string{cwd}, additionalDirectories...) {
		if root != "" {
			projects[root] = map[string]any{"trust_level": "trusted"}
		}
	}
	if len(projects) > 0 {
		cfg["projects"] = projects
	}
	if len(additionalDirectories) > 0 {
		cfg["sandbox_workspace_write"] = map[string]any{"writable_roots": additionalDirectories}
	}
	if mcp := mcpServersConfig(servers); len(mcp) > 0 {
		cfg["mcp_servers"] = mcp
	}
	if len(cfg) == 0 {
		return nil
	}
	return cfg
}

func mcpServersConfig(servers []acpsdk.McpServer) map[string]any {
	out := map[string]any{}
	for _, s := range servers {
		switch {
		case s.Stdio != nil:
			env := map[string]string{}
			for _, e := range s.Stdio.Env {
				env[e.Name] = e.Value
			}
			out[s.Stdio.Name] = map[string]any{"command": s.Stdio.Command, "args": s.Stdio.Args, "env": env}
		case s.Http != nil:
			headers := map[string]string{}
			for _, h := range s.Http.Headers {
				headers[h.Name] = h.Value
			}
			out[s.Http.Name] = map[string]any{"url": s.Http.Url, "http_headers": headers}
		}
	}
	return out
}

func (a *Agent) Prompt(ctx context.Context, params acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	s := a.lookup(params.SessionId)
	if s == nil {
		return acpsdk.PromptResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	select {
	case s.promptGate <- struct{}{}:
		defer func() { <-s.promptGate }()
	case <-ctx.Done():
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, UserMessageId: params.MessageId}, nil
	case <-s.closedCh:
		return acpsdk.PromptResponse{}, fmt.Errorf("session %s is closed", params.SessionId)
	}
	if s.isClosed() {
		return acpsdk.PromptResponse{}, fmt.Errorf("session %s is closed", params.SessionId)
	}
	if ctx.Err() != nil {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, UserMessageId: params.MessageId}, nil
	}
	if handled, err := a.handleCommand(ctx, s, params.Prompt); handled {
		if err != nil {
			return acpsdk.PromptResponse{}, err
		}
		return acpsdk.PromptResponse{
			StopReason:    acpsdk.StopReasonEndTurn,
			UserMessageId: params.MessageId,
		}, nil
	}
	stop, usage, err := s.runTurn(ctx, a.connection(), a.codex, a.clientCapabilities, a.models, params.Prompt)
	if err != nil {
		return acpsdk.PromptResponse{}, err
	}
	return acpsdk.PromptResponse{StopReason: stop, Usage: usage, UserMessageId: params.MessageId}, nil
}

type builtinCommand struct {
	name string
	args string
}

func parseBuiltinCommand(prompt []acpsdk.ContentBlock) (builtinCommand, bool) {
	if len(prompt) != 1 || prompt[0].Text == nil {
		return builtinCommand{}, false
	}
	text := strings.TrimSpace(prompt[0].Text.Text)
	if !strings.HasPrefix(text, "/") {
		return builtinCommand{}, false
	}
	command := strings.TrimSpace(strings.TrimPrefix(text, "/"))
	if command == "" {
		return builtinCommand{}, false
	}
	fields := strings.Fields(command)
	name := fields[0]
	return builtinCommand{name: strings.ToLower(name), args: strings.TrimSpace(command[len(name):])}, true
}

func (a *Agent) handleCommand(ctx context.Context, s *session, prompt []acpsdk.ContentBlock) (bool, error) {
	command, ok := parseBuiltinCommand(prompt)
	if !ok {
		return false, nil
	}
	switch command.name {
	case "plan":
		if command.args != "" {
			return false, nil
		}
		return true, a.togglePlanMode(ctx, s)
	case "rename":
		if command.args == "" {
			if conn := a.connection(); conn != nil {
				return true, notifyClient(ctx, conn, s.id, acpsdk.UpdateAgentMessageText("Usage: /rename <new name>"))
			}
			return true, nil
		}
		if err := a.codex.threadSetName(ctx, threadSetNameParams{ThreadID: string(s.id), Name: command.args}); err != nil {
			return true, fmt.Errorf("thread/name/set: %w", err)
		}
		return true, nil
	default:
		return false, nil
	}
}

func (a *Agent) togglePlanMode(ctx context.Context, s *session) error {
	s.mu.Lock()
	modelID := s.modelID
	effort := s.effort
	next := planCollaborationMode
	if s.collaborationMode == planCollaborationMode {
		next = defaultCollaborationMode
	}
	s.mu.Unlock()

	if err := a.codex.threadSettingsUpdate(ctx, newThreadSettingsUpdate(string(s.id), modelID, effort, next)); err != nil {
		return fmt.Errorf("thread/settings/update: %w", err)
	}
	s.mu.Lock()
	s.collaborationMode = next
	s.mu.Unlock()
	if conn := a.connection(); conn != nil {
		if err := notifyClient(ctx, conn, s.id, acpsdk.SessionUpdate{ConfigOptionUpdate: &acpsdk.SessionConfigOptionUpdate{
			SessionUpdate: "config_option_update",
			ConfigOptions: buildConfigOptions(a.models, modelID, effort, next),
		}}); err != nil {
			return err
		}
		label := "Plan mode enabled."
		if next == defaultCollaborationMode {
			label = "Plan mode disabled."
		}
		return notifyClient(ctx, conn, s.id, acpsdk.UpdateAgentMessageText(label))
	}
	return nil
}

func (a *Agent) Steer(ctx context.Context, sessionID acpsdk.SessionId, prompt []acpsdk.ContentBlock, messageID string) error {
	s := a.lookup(sessionID)
	if s == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return s.steer(ctx, a.codex, prompt, messageID)
}

func (a *Agent) Cancel(ctx context.Context, params acpsdk.CancelNotification) error {
	if s := a.lookup(params.SessionId); s != nil {
		a.interruptSession(ctx, s)
	}
	return nil
}

func (a *Agent) interruptSession(ctx context.Context, s *session) {
	interruptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	s.interrupt(interruptCtx, a.codex)
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
	s.mode = id
	s.mu.Unlock()
	return acpsdk.SetSessionModeResponse{}, nil
}

func (a *Agent) SetSessionConfigOption(ctx context.Context, params acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
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
	modelID := s.modelID
	effort := s.effort
	collaborationMode := s.collaborationMode
	s.mu.Unlock()

	switch string(v.ConfigId) {
	case modelConfigID:
		m := findModel(a.models, value)
		if m == nil {
			return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown model %q", value)
		}
		modelID = value
		if !acp.IsValidEffort(m.EffortLevels, effort) {
			effort = "default"
		}
	case effortConfigID:
		m := findModel(a.models, modelID)
		if m == nil || !acp.IsValidEffort(m.EffortLevels, value) {
			return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("effort %q invalid for model %s", value, modelID)
		}
		effort = value
	case collaborationModeConfigID:
		if !isValidCollaborationMode(value) {
			return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown collaboration mode %q", value)
		}
		collaborationMode = value
	default:
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown configId %q", v.ConfigId)
	}

	if collaborationMode == planCollaborationMode || string(v.ConfigId) == collaborationModeConfigID {
		if err := a.codex.threadSettingsUpdate(ctx, newThreadSettingsUpdate(string(s.id), modelID, effort, collaborationMode)); err != nil {
			return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("thread/settings/update: %w", err)
		}
	}
	s.mu.Lock()
	s.modelID = modelID
	s.effort = effort
	s.collaborationMode = collaborationMode
	s.mu.Unlock()
	return acpsdk.SetSessionConfigOptionResponse{
		ConfigOptions: buildConfigOptions(a.models, modelID, effort, collaborationMode),
	}, nil
}

func newThreadSettingsUpdate(threadID, modelID, effort, collaborationMode string) threadSettingsUpdateParams {
	var reasoningEffort *string
	if effort != "" && effort != "default" {
		reasoningEffort = &effort
	}
	return threadSettingsUpdateParams{
		ThreadID: threadID,
		CollaborationMode: codexCollaborationMode{
			Mode: collaborationMode,
			Settings: collaborationModeSettings{
				Model:           modelID,
				ReasoningEffort: reasoningEffort,
			},
		},
	}
}

func (a *Agent) CloseSession(ctx context.Context, params acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	a.mu.Lock()
	s := a.sessions[params.SessionId]
	delete(a.sessions, params.SessionId)
	a.mu.Unlock()
	if s != nil {
		s.markClosed()
		a.interruptSession(ctx, s)
	}
	_ = a.codex.threadUnsubscribe(ctx, threadUnsubscribeParams{ThreadID: string(params.SessionId)})
	return acpsdk.CloseSessionResponse{}, nil
}

func (a *Agent) UnstableDeleteSession(ctx context.Context, params acpsdk.UnstableDeleteSessionRequest) (acpsdk.UnstableDeleteSessionResponse, error) {
	a.mu.Lock()
	s := a.sessions[params.SessionId]
	delete(a.sessions, params.SessionId)
	a.mu.Unlock()
	if s != nil {
		s.markClosed()
		a.interruptSession(ctx, s)
	}
	_ = a.codex.threadUnsubscribe(ctx, threadUnsubscribeParams{ThreadID: string(params.SessionId)})
	if err := a.codex.threadArchive(ctx, threadArchiveParams{ThreadID: string(params.SessionId)}); err != nil {
		// ACP session/delete is idempotent: deleting an unknown, unprompted,
		// or already-archived thread succeeds.
		if isUnknownThreadError(err) {
			return acpsdk.UnstableDeleteSessionResponse{}, nil
		}
		return acpsdk.UnstableDeleteSessionResponse{}, fmt.Errorf("thread/archive: %w", err)
	}
	return acpsdk.UnstableDeleteSessionResponse{}, nil
}

func (a *Agent) ListSessions(ctx context.Context, params acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
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
		return acpsdk.ListSessionsResponse{}, fmt.Errorf("thread/list: %w", err)
	}

	sessions := make([]acpsdk.SessionInfo, 0, len(resp.Data))
	for _, t := range resp.Data {
		sessions = append(sessions, acpsdk.SessionInfo{
			SessionId: acpsdk.SessionId(t.ID),
			Cwd:       t.Cwd,
			Title:     sessionTitle(t),
			UpdatedAt: sessionUpdatedAt(t.UpdatedAt),
		})
	}
	return acpsdk.ListSessionsResponse{Sessions: sessions, NextCursor: resp.NextCursor}, nil
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

func (a *Agent) ResumeSession(ctx context.Context, params acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	if err := acp.ValidateMCPServers(params.McpServers, acpsdk.McpCapabilities{Http: true}); err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	cwd, additional, err := acp.NormalizeSessionRoots(params.Cwd, params.AdditionalDirectories)
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	mcpVersion := a.codex.currentMCPStartupVersion()
	resp, materialized, err := a.codex.threadResumeOrRead(ctx, threadResumeParams{
		ThreadID:      string(params.SessionId),
		ExcludeTurns:  true,
		Cwd:           cwd,
		ModelProvider: a.resumeModelProvider(ctx),
		Config:        sessionConfig(cwd, additional, params.McpServers),
	})
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, sessionResumeError(err)
	}
	if err := a.awaitMCPStartup(ctx, string(params.SessionId), params.Meta, params.McpServers, mcpVersion); err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	s := a.registerResumedSession(params.SessionId, resp, materialized, additional)
	if err := a.sendAvailableCommands(ctx, s.id); err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	return acpsdk.ResumeSessionResponse{
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort, s.collaborationMode),
	}, nil
}

func (a *Agent) LoadSession(ctx context.Context, params acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	if err := acp.ValidateMCPServers(params.McpServers, acpsdk.McpCapabilities{Http: true}); err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	cwd, additional, err := acp.NormalizeSessionRoots(params.Cwd, params.AdditionalDirectories)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	resp, materialized, err := a.codex.threadResumeOrRead(ctx, threadResumeParams{
		ThreadID:      string(params.SessionId),
		ExcludeTurns:  true,
		Cwd:           cwd,
		ModelProvider: a.resumeModelProvider(ctx),
		Config:        sessionConfig(cwd, additional, params.McpServers),
	})
	if err != nil {
		return acpsdk.LoadSessionResponse{}, sessionResumeError(err)
	}
	thread := resp.Thread
	thread.Turns = nil
	if materialized {
		read, err := a.codex.threadReadWithHistory(ctx, string(params.SessionId))
		if err != nil {
			return acpsdk.LoadSessionResponse{}, fmt.Errorf("read session history: %w", err)
		}
		thread = read.Thread
	}
	s := a.registerResumedSession(params.SessionId, resp, materialized, additional)
	if err := a.sendAvailableCommands(ctx, s.id); err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	outputs := rolloutCommandOutputs(string(params.SessionId), threadPath(thread))
	if err := streamThreadHistory(ctx, a.connection(), s.id, thread.Turns, outputs); err != nil {
		return acpsdk.LoadSessionResponse{}, fmt.Errorf("send session history: %w", err)
	}
	return acpsdk.LoadSessionResponse{
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: buildConfigOptions(a.models, s.modelID, s.effort, s.collaborationMode),
	}, nil
}

func (a *Agent) UnstableForkSession(ctx context.Context, params acpsdk.UnstableForkSessionRequest) (acpsdk.UnstableForkSessionResponse, error) {
	servers, err := acp.StableMCPServers(params.McpServers, acpsdk.McpCapabilities{Http: true})
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	cwd, additional, err := acp.NormalizeSessionRoots(params.Cwd, params.AdditionalDirectories)
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	mcpVersion := a.codex.currentMCPStartupVersion()
	resp, err := a.codex.threadFork(ctx, threadForkParams{
		ThreadID:      string(params.SessionId),
		ExcludeTurns:  true,
		Cwd:           cwd,
		ModelProvider: a.resumeModelProvider(ctx),
		Config:        sessionConfig(cwd, additional, servers),
	})
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, fmt.Errorf("thread/fork: %w", err)
	}
	if resp.Thread.ID == "" {
		return acpsdk.UnstableForkSessionResponse{}, fmt.Errorf("codex returned empty forked thread id")
	}
	if err := a.awaitMCPStartup(ctx, resp.Thread.ID, params.Meta, servers, mcpVersion); err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}

	s := a.registerSession(acpsdk.SessionId(resp.Thread.ID), resp.Model, derefEffort(resp.ReasoningEffort), defaultCollaborationMode, additional)
	if source := a.lookup(params.SessionId); source != nil {
		source.mu.Lock()
		mode, collaborationMode := source.mode, source.collaborationMode
		source.mu.Unlock()
		s.mu.Lock()
		s.mode = mode
		s.mu.Unlock()
		// Codex starts forks in default mode; report plan only once the fork has it.
		if collaborationMode == planCollaborationMode &&
			a.codex.threadSettingsUpdate(ctx, newThreadSettingsUpdate(string(s.id), s.modelID, s.effort, collaborationMode)) == nil {
			s.mu.Lock()
			s.collaborationMode = collaborationMode
			s.mu.Unlock()
		}
	}
	if err := a.sendAvailableCommands(ctx, s.id); err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	return acpsdk.UnstableForkSessionResponse{
		SessionId:     s.id,
		Modes:         buildSessionModeState(s.mode),
		ConfigOptions: acp.UnstableConfigOptions(buildConfigOptions(a.models, s.modelID, s.effort, s.collaborationMode)),
	}, nil
}

func threadPath(t threadInfo) string {
	if t.Path != nil {
		return *t.Path
	}
	return ""
}
