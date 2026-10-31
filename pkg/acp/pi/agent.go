package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
	picli "github.com/adrianliechti/wingman-agent/pkg/external/pi"
)

type Options struct {
	Path string
	Env  []string
	Args []string

	Stderr io.Writer

	SessionsDir string
}

type Agent struct {
	conn acp.ClientConnection
	opts Options

	mu       sync.Mutex
	sessions map[acpsdk.SessionId]*session
}

var _ acpsdk.Agent = (*Agent)(nil)

func New(opts Options) *Agent {
	if opts.Path == "" {
		opts.Path = picli.BinPath()
	}
	opts.Env = picli.BuildEnv(opts.Env, "")
	return &Agent{
		opts:     opts,
		sessions: map[acpsdk.SessionId]*session{},
	}
}

func (a *Agent) SetClientConnection(conn acp.ClientConnection) { a.conn = conn }

func (a *Agent) lookup(id acpsdk.SessionId) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

func (a *Agent) Close() error {
	a.mu.Lock()
	sessions := a.sessions
	a.sessions = map[acpsdk.SessionId]*session{}
	a.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	return nil
}

func (a *Agent) Initialize(context.Context, acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	return acpsdk.InitializeResponse{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		AgentInfo: &acpsdk.Implementation{
			Name:    "pi-acp",
			Title:   new("Pi (ACP)"),
			Version: "0.1.0",
		},
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession: a.opts.SessionsDir != "",
			PromptCapabilities: acpsdk.PromptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
			SessionCapabilities: acpsdk.SessionCapabilities{
				Close:  &acpsdk.SessionCloseCapabilities{},
				Resume: a.resumeCapability(),
				Fork:   a.forkCapability(),
				List:   a.listCapability(),
				Delete: a.deleteCapability(),
			},
		},
	}, nil
}

func (a *Agent) resumeCapability() *acpsdk.SessionResumeCapabilities {
	if a.opts.SessionsDir == "" {
		return nil
	}
	return &acpsdk.SessionResumeCapabilities{}
}

func (a *Agent) forkCapability() *acpsdk.SessionForkCapabilities {
	if a.opts.SessionsDir == "" {
		return nil
	}
	return &acpsdk.SessionForkCapabilities{}
}

func (a *Agent) listCapability() *acpsdk.SessionListCapabilities {
	if a.opts.SessionsDir == "" {
		return nil
	}
	return &acpsdk.SessionListCapabilities{}
}

func (a *Agent) deleteCapability() *acpsdk.SessionDeleteCapabilities {
	if a.opts.SessionsDir == "" {
		return nil
	}
	return &acpsdk.SessionDeleteCapabilities{}
}

// UnstableDeleteSession is idempotent per the ACP session/delete semantics:
// deleting a session that does not exist (or is already gone) succeeds.
func (a *Agent) UnstableDeleteSession(_ context.Context, params acpsdk.UnstableDeleteSessionRequest) (acpsdk.UnstableDeleteSessionResponse, error) {
	a.disposeSession(params.SessionId)
	if a.opts.SessionsDir != "" {
		if file, ok := findSessionFile(a.opts.SessionsDir, string(params.SessionId)); ok {
			if err := os.Remove(file.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return acpsdk.UnstableDeleteSessionResponse{}, err
			}
		}
	}
	return acpsdk.UnstableDeleteSessionResponse{}, nil
}

func (a *Agent) Authenticate(context.Context, acpsdk.AuthenticateRequest) (acpsdk.AuthenticateResponse, error) {
	return acpsdk.AuthenticateResponse{}, nil
}

func (a *Agent) Logout(context.Context, acpsdk.LogoutRequest) (acpsdk.LogoutResponse, error) {
	return acpsdk.LogoutResponse{}, nil
}

func (a *Agent) NewSession(ctx context.Context, params acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	cwd, _, err := acp.NormalizeSessionRoots(params.Cwd, nil)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}

	proc, bridge, err := a.spawnSessionProcess(ctx, cwd, nil, params.McpServers)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}

	modelsData, err := proc.getAvailableModels(ctx)
	if err != nil {
		proc.dispose()
		bridge.cleanup()
		return acpsdk.NewSessionResponse{}, err
	}
	models := parseAvailableModels(modelsData)
	if len(models) == 0 {
		proc.dispose()
		bridge.cleanup()
		return acpsdk.NewSessionResponse{}, acpsdk.NewAuthRequired(nil)
	}

	stateData, err := proc.getState(ctx)
	if err != nil {
		proc.dispose()
		bridge.cleanup()
		return acpsdk.NewSessionResponse{}, err
	}
	state := parseState(stateData)

	id := acpsdk.SessionId(state.SessionID)
	if id == "" {
		id = acpsdk.SessionId(uuid.NewString())
	}

	s := newSession(id, cwd, proc)
	s.cleanup = bridge.cleanup
	s.models = models
	s.autoTitle = true
	if err := s.refreshConfiguration(ctx); err != nil {
		s.close()
		return acpsdk.NewSessionResponse{}, err
	}

	a.storeSession(s)
	a.publishInitialUsage(s)

	return acpsdk.NewSessionResponse{
		SessionId:     id,
		ConfigOptions: s.configOptions(),
	}, nil
}

func (a *Agent) Prompt(ctx context.Context, params acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	s := a.lookup(params.SessionId)
	if s == nil {
		return acpsdk.PromptResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	if err := s.promptMu.Lock(ctx); err != nil {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
	}
	defer s.promptMu.Unlock()
	if s.isClosed() {
		return acpsdk.PromptResponse{}, fmt.Errorf("session %s is closed", params.SessionId)
	}
	if ctx.Err() != nil {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, UserMessageId: params.MessageId}, nil
	}
	stop, err := s.runTurn(ctx, a.conn, params.Prompt)
	if err != nil {
		return acpsdk.PromptResponse{}, err
	}
	return acpsdk.PromptResponse{StopReason: stop, UserMessageId: params.MessageId}, nil
}

func (a *Agent) Cancel(_ context.Context, params acpsdk.CancelNotification) error {
	if s := a.lookup(params.SessionId); s != nil {
		s.cancel()
	}
	return nil
}

func (a *Agent) SetSessionMode(context.Context, acpsdk.SetSessionModeRequest) (acpsdk.SetSessionModeResponse, error) {
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

	switch string(v.ConfigId) {
	case modelConfigID:
		provider, modelID, ok := strings.Cut(value, "/")
		if !ok {
			return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("invalid model id %q", value)
		}
		if err := s.proc.setModel(ctx, provider, modelID); err != nil {
			return acpsdk.SetSessionConfigOptionResponse{}, err
		}
		if err := s.refreshConfiguration(ctx); err != nil {
			return acpsdk.SetSessionConfigOptionResponse{}, err
		}
		// Models differ in context window size.
		s.publishContextUsage(ctx, a.conn)

	case effortConfigID:
		s.mu.Lock()
		valid := isThinkingLevel(s.thinkingLevels, value)
		s.mu.Unlock()
		if !valid {
			return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown thinking level %q", value)
		}
		if err := s.proc.setThinkingLevel(ctx, value); err != nil {
			return acpsdk.SetSessionConfigOptionResponse{}, err
		}
		if err := s.refreshConfiguration(ctx); err != nil {
			return acpsdk.SetSessionConfigOptionResponse{}, err
		}

	default:
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown configId %q", v.ConfigId)
	}

	return acpsdk.SetSessionConfigOptionResponse{ConfigOptions: s.configOptions()}, nil
}

func (a *Agent) CloseSession(_ context.Context, params acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	a.disposeSession(params.SessionId)
	return acpsdk.CloseSessionResponse{}, nil
}

func (a *Agent) disposeSession(id acpsdk.SessionId) {
	a.mu.Lock()
	s := a.sessions[id]
	delete(a.sessions, id)
	a.mu.Unlock()
	if s != nil {
		s.close()
	}
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

const sessionPageSize = 50

func (a *Agent) ListSessions(_ context.Context, params acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	if a.opts.SessionsDir == "" {
		return acpsdk.ListSessionsResponse{Sessions: []acpsdk.SessionInfo{}}, nil
	}

	all := listSessionFiles(a.opts.SessionsDir)

	if params.Cwd != nil && *params.Cwd != "" {
		// pi records its own symlink-resolved cwd (e.g. /private/var vs
		// /var on macOS), so compare canonical forms.
		want := canonicalPath(*params.Cwd)
		filtered := all[:0]
		for _, s := range all {
			if canonicalPath(s.Cwd) == want {
				filtered = append(filtered, s)
			}
		}
		all = filtered
	}

	offset := 0
	if params.Cursor != nil {
		if n, err := strconv.Atoi(*params.Cursor); err == nil && n > 0 {
			offset = n
		}
	}
	if offset > len(all) {
		offset = len(all)
	}
	end := min(offset+sessionPageSize, len(all))

	sessions := make([]acpsdk.SessionInfo, 0, end-offset)
	for _, s := range all[offset:end] {
		info := acpsdk.SessionInfo{SessionId: acpsdk.SessionId(s.ID), Cwd: s.Cwd}
		if s.Title != "" {
			title := s.Title
			info.Title = &title
		}
		if s.UpdatedAt != "" {
			updated := s.UpdatedAt
			info.UpdatedAt = &updated
		}
		sessions = append(sessions, info)
	}

	var nextCursor *string
	if end < len(all) {
		c := strconv.Itoa(end)
		nextCursor = &c
	}

	return acpsdk.ListSessionsResponse{Sessions: sessions, NextCursor: nextCursor}, nil
}

func (a *Agent) ResumeSession(ctx context.Context, params acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	response, err := a.loadSession(ctx, acpsdk.LoadSessionRequest{SessionId: params.SessionId, Cwd: params.Cwd, McpServers: params.McpServers}, false)
	return acpsdk.ResumeSessionResponse{ConfigOptions: response.ConfigOptions}, err
}

func (a *Agent) LoadSession(ctx context.Context, params acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	return a.loadSession(ctx, params, true)
}

func (a *Agent) loadSession(ctx context.Context, params acpsdk.LoadSessionRequest, replay bool) (acpsdk.LoadSessionResponse, error) {
	cwd, _, err := acp.NormalizeSessionRoots(params.Cwd, nil)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	if err := acp.ValidateMCPServers(params.McpServers, acpsdk.McpCapabilities{}); err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	if s := a.lookup(params.SessionId); s != nil && !s.isClosed() && s.cwd == cwd && len(params.McpServers) == 0 {
		if err := a.restoreUpdates(ctx, s, replay); err != nil {
			return acpsdk.LoadSessionResponse{}, err
		}
		return acpsdk.LoadSessionResponse{ConfigOptions: s.configOptions()}, nil
	}
	if a.opts.SessionsDir == "" {
		return acpsdk.LoadSessionResponse{}, errors.ErrUnsupported
	}
	file, ok := findSessionFile(a.opts.SessionsDir, string(params.SessionId))
	if !ok {
		return acpsdk.LoadSessionResponse{}, fmt.Errorf("unknown session %s", params.SessionId)
	}

	proc, bridge, err := a.spawnSessionProcess(ctx, cwd, []string{"--session", file.Path}, params.McpServers)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}

	modelsData, err := proc.getAvailableModels(ctx)
	if err != nil {
		proc.dispose()
		bridge.cleanup()
		return acpsdk.LoadSessionResponse{}, err
	}
	models := parseAvailableModels(modelsData)
	if len(models) == 0 {
		proc.dispose()
		bridge.cleanup()
		return acpsdk.LoadSessionResponse{}, acpsdk.NewAuthRequired(nil)
	}

	s := newSession(params.SessionId, cwd, proc)
	s.cleanup = bridge.cleanup
	s.models = models
	if err := s.refreshConfiguration(ctx); err != nil {
		s.close()
		return acpsdk.LoadSessionResponse{}, err
	}

	send := func(u acpsdk.SessionUpdate) {
		_ = acp.Notify(ctx, a.conn, s.id, u)
	}
	if err := a.restoreUpdates(ctx, s, replay); err != nil {
		s.close()
		return acpsdk.LoadSessionResponse{}, err
	}
	a.storeSession(s)
	if u := s.titleUpdate(s.restoreTitle(file.Path), false); u != nil {
		send(*u)
	}
	s.publishContextUsage(ctx, a.conn)

	return acpsdk.LoadSessionResponse{ConfigOptions: s.configOptions()}, nil
}

func (a *Agent) restoreUpdates(ctx context.Context, s *session, replay bool) error {
	if !replay {
		return nil
	}
	data, err := s.proc.getMessages(ctx)
	if err != nil {
		return err
	}
	var sendErr error
	replayMessages(func(update acpsdk.SessionUpdate) {
		if sendErr == nil {
			sendErr = acp.Notify(ctx, a.conn, s.id, update)
		}
	}, data, s.cwd)
	return sendErr
}

func (a *Agent) publishInitialUsage(s *session) {
	if a.conn == nil {
		return
	}
	// Asynchronous so it normally follows the response that tells the client the session id.
	go func() {
		if a.lookup(s.id) == s {
			s.publishContextUsage(context.Background(), a.conn)
		}
	}()
}

func (a *Agent) spawnSessionProcess(ctx context.Context, cwd string, extraArgs []string, servers []acpsdk.McpServer) (*process, *preparedMCPBridge, error) {
	bridge, err := prepareMCPBridge(cwd, a.opts.Env, servers)
	if err != nil {
		return nil, nil, err
	}

	args := append([]string(nil), a.opts.Args...)
	args = append(args, extraArgs...)
	args = append(args, bridge.args...)
	proc, err := spawn(spawnOptions{Path: a.opts.Path, Dir: cwd, Env: bridge.env, Args: args, Stderr: a.opts.Stderr})
	if err != nil {
		bridge.cleanup()
		return nil, nil, err
	}
	if err := bridge.waitReady(ctx, proc); err != nil {
		proc.dispose()
		bridge.cleanup()
		return nil, nil, err
	}
	return proc, bridge, nil
}

func (a *Agent) UnstableForkSession(ctx context.Context, params acpsdk.UnstableForkSessionRequest) (acpsdk.UnstableForkSessionResponse, error) {
	if a.opts.SessionsDir == "" {
		return acpsdk.UnstableForkSessionResponse{}, errors.ErrUnsupported
	}
	cwd, _, err := acp.NormalizeSessionRoots(params.Cwd, nil)
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	servers, err := stableMCPServers(params.McpServers)
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	file, ok := findSessionFile(a.opts.SessionsDir, string(params.SessionId))
	if !ok {
		return a.forkUnpersistedSession(ctx, params.SessionId, cwd, servers)
	}

	proc, bridge, err := a.spawnSessionProcess(ctx, cwd, []string{"--session", file.Path}, servers)
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	fail := func(err error) (acpsdk.UnstableForkSessionResponse, error) {
		proc.dispose()
		bridge.cleanup()
		return acpsdk.UnstableForkSessionResponse{}, err
	}

	if err := bridge.resetReady(); err != nil {
		return fail(err)
	}
	cloneData, err := proc.clone(ctx)
	if err != nil {
		return fail(err)
	}
	if err := bridge.waitReady(ctx, proc); err != nil {
		return fail(err)
	}
	var cloneResult struct {
		Cancelled bool `json:"cancelled"`
	}
	if json.Unmarshal(cloneData, &cloneResult) != nil {
		return fail(errors.New("pi clone returned an invalid response"))
	}
	if cloneResult.Cancelled {
		return fail(errors.New("pi clone was cancelled"))
	}

	modelsData, err := proc.getAvailableModels(ctx)
	if err != nil {
		return fail(err)
	}
	models := parseAvailableModels(modelsData)
	if len(models) == 0 {
		return fail(acpsdk.NewAuthRequired(nil))
	}
	stateData, err := proc.getState(ctx)
	if err != nil {
		return fail(err)
	}
	state := parseState(stateData)
	id := acpsdk.SessionId(state.SessionID)
	if id == "" || id == params.SessionId {
		return fail(fmt.Errorf("pi clone returned invalid session id %q", id))
	}

	s := newSession(id, cwd, proc)
	s.cleanup = bridge.cleanup
	s.models = models
	if err := s.refreshConfiguration(ctx); err != nil {
		return fail(err)
	}

	a.storeSession(s)
	a.publishInitialUsage(s)
	return acpsdk.UnstableForkSessionResponse{SessionId: id, ConfigOptions: acp.UnstableConfigOptions(s.configOptions())}, nil
}

// Pi does not create a session file until the first assistant message. An empty
// live session can still be forked by starting one with the same configuration.
func (a *Agent) forkUnpersistedSession(ctx context.Context, id acpsdk.SessionId, cwd string, servers []acpsdk.McpServer) (acpsdk.UnstableForkSessionResponse, error) {
	source := a.lookup(id)
	if source == nil || source.isClosed() {
		return acpsdk.UnstableForkSessionResponse{}, fmt.Errorf("unknown session %s", id)
	}
	if err := source.promptMu.Lock(ctx); err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	data, err := source.proc.getMessages(ctx)
	source.mu.Lock()
	model, thinking := source.currentModel, source.thinking
	source.mu.Unlock()
	source.promptMu.Unlock()
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	var history struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(data, &history) != nil || history.Messages == nil || len(history.Messages) != 0 {
		return acpsdk.UnstableForkSessionResponse{}, fmt.Errorf("session %s has unpersisted history; wait for its turn to finish before forking", id)
	}
	created, err := a.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: cwd, McpServers: servers})
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	s := a.lookup(created.SessionId)
	if provider, modelID, ok := strings.Cut(model, "/"); ok {
		err = s.proc.setModel(ctx, provider, modelID)
	}
	if err == nil && thinking != "" {
		err = s.proc.setThinkingLevel(ctx, thinking)
	}
	if err == nil {
		err = s.refreshConfiguration(ctx)
	}
	if err != nil {
		a.disposeSession(s.id)
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	return acpsdk.UnstableForkSessionResponse{SessionId: s.id, ConfigOptions: acp.UnstableConfigOptions(s.configOptions())}, nil
}

func stableMCPServers(servers []acpsdk.UnstableMcpServer) ([]acpsdk.McpServer, error) {
	return acp.StableMCPServers(servers, acpsdk.McpCapabilities{})
}

func (a *Agent) SetAgentConnection(conn *acpsdk.AgentSideConnection) { a.SetClientConnection(conn) }
