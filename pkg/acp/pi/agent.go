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
	conn *acpsdk.AgentSideConnection
	opts Options

	mu       sync.Mutex
	sessions map[acpsdk.SessionId]*session
}

var _ acpsdk.Agent = (*Agent)(nil)

func New(opts Options) *Agent {
	if opts.Path == "" {
		opts.Path = picli.BinPath()
	}
	return &Agent{
		opts:     opts,
		sessions: map[acpsdk.SessionId]*session{},
	}
}

func (a *Agent) SetAgentConnection(conn *acpsdk.AgentSideConnection) { a.conn = conn }

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
				Fork:   a.forkCapability(),
				List:   a.listCapability(),
				Delete: a.deleteCapability(),
			},
		},
	}, nil
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
	if err := s.refreshConfiguration(ctx); err != nil {
		s.close()
		return acpsdk.NewSessionResponse{}, err
	}

	a.storeSession(s)

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
		return acpsdk.ListSessionsResponse{}, nil
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

func (a *Agent) ResumeSession(context.Context, acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	return acpsdk.ResumeSessionResponse{}, errors.ErrUnsupported
}

func (a *Agent) LoadSession(ctx context.Context, params acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	if a.opts.SessionsDir == "" {
		return acpsdk.LoadSessionResponse{}, errors.ErrUnsupported
	}
	file, ok := findSessionFile(a.opts.SessionsDir, string(params.SessionId))
	if !ok {
		return acpsdk.LoadSessionResponse{}, fmt.Errorf("unknown session %s", params.SessionId)
	}

	cwd, _, err := acp.NormalizeSessionRoots(params.Cwd, nil)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
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

	a.storeSession(s)

	send := func(u acpsdk.SessionUpdate) {
		_ = a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: s.id, Update: u})
	}
	if data, err := proc.getMessages(ctx); err == nil {
		replayMessages(send, data, cwd)
	}

	return acpsdk.LoadSessionResponse{ConfigOptions: s.configOptions()}, nil
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
	file, ok := findSessionFile(a.opts.SessionsDir, string(params.SessionId))
	if !ok {
		return acpsdk.UnstableForkSessionResponse{}, fmt.Errorf("unknown session %s", params.SessionId)
	}
	cwd, _, err := acp.NormalizeSessionRoots(params.Cwd, nil)
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
	}
	servers, err := stableMCPServers(params.McpServers)
	if err != nil {
		return acpsdk.UnstableForkSessionResponse{}, err
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
	return acpsdk.UnstableForkSessionResponse{SessionId: id}, nil
}

func stableMCPServers(servers []acpsdk.UnstableMcpServer) ([]acpsdk.McpServer, error) {
	return acp.StableMCPServers(servers, acpsdk.McpCapabilities{})
}
