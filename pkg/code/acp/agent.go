// Package acp is the ACP-subprocess [code.Agent] implementation. One
// Agent spawns and owns a single ACP server subprocess; that connection
// hosts every wingman session for the chosen backend (codex / claude /
// ...). The session ids the interface deals in are the ACP server's
// own session ids — no wingman-side translation.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

type Agent struct {
	workspace *code.Workspace
	def       code.AgentDef

	cmd       *exec.Cmd
	stdin     io.WriteCloser
	conn      *acpsdk.ClientSideConnection
	closeOnce sync.Once

	// inProcess wiring (NewInProcess only). nil for the subprocess path.
	serverDone <-chan struct{}
	cleanup    func() error

	caps acpsdk.AgentCapabilities

	// ui approves permission requests the ACP server raises mid-turn. nil → approve.
	ui code.UI

	// configMu guards the model + effort catalog. These are global to the
	// connection because the code.Agent interface exposes Models()/Effort()
	// without a session id. Modes, which the interface does scope per session,
	// live on sessionState instead.
	configMu   sync.RWMutex
	models     []code.Model
	modelID    string
	effortID   string
	effortOpts []string

	mu       sync.Mutex
	sessions map[string]*sessionState
}

type sessionState struct {
	parent *Agent
	id     acpsdk.SessionId

	mu       sync.Mutex
	messages []agent.Message
	usage    agent.Usage
	inflight *turn

	toolCallsMu sync.Mutex
	toolCalls   map[string]toolCall

	// modes are per-session in ACP (session/new + session/load report them per
	// session, set_mode and current_mode_update are session-scoped). Guarded by
	// mu above so concurrent sessions don't clobber each other's current mode.
	modes  []code.Mode
	modeID string
}

type toolCall struct {
	name string
	args string
}

type turn struct {
	events chan event
	done   chan struct{}
	cancel context.CancelFunc

	mu      sync.Mutex
	emitted []agent.Message // role-sliced; committed to session.messages on close
}

type event struct {
	msg  agent.Message
	err  error
	done bool
}

const (
	modelConfigID  = "model"
	effortConfigID = "effort"
	initTimeout    = 30 * time.Second
)

// New spawns the configured subprocess and runs the ACP handshake.
func New(ws *code.Workspace, def code.AgentDef) (*Agent, error) {
	if def.Command == "" {
		return nil, fmt.Errorf("agent %q: empty command", def.Name)
	}

	cwd := ws.RootPath
	cmd := exec.Command(def.Command, def.Args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for k, v := range def.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("agent %q: stdin pipe: %w", def.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("agent %q: stdout pipe: %w", def.Name, err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("agent %q: start: %w", def.Name, err)
	}

	a := &Agent{
		workspace: ws,
		def:       def,
		cmd:       cmd,
		stdin:     stdin,
		sessions:  map[string]*sessionState{},
	}
	a.conn = acpsdk.NewClientSideConnection(a, stdin, stdout)
	// SDK logs every connection teardown at INFO. Each agent swap +
	// shutdown produces one; keep warnings and errors, drop the trace.
	a.conn.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))

	initCtx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	resp, err := a.conn.Initialize(initCtx, acpsdk.InitializeRequest{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		ClientCapabilities: acpsdk.ClientCapabilities{
			Fs: acpsdk.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
	})
	if err != nil {
		a.shutdown()
		return nil, fmt.Errorf("agent %q: initialize: %w", def.Name, err)
	}
	a.caps = resp.AgentCapabilities
	return a, nil
}

// NewInProcess wires an in-memory ACP-server agent (e.g. *claude.Agent,
// *codex.Agent) into a code.Agent backend using io.Pipe pairs — no
// subprocess for the protocol itself. setupServer is invoked with the
// server-side AgentSideConnection so libraries that need it (claude,
// codex) can call SetAgentConnection before the initialize handshake.
// cleanup runs during Close after the connection is torn down; pass nil
// for libraries with no resources to release (codex.Spawn returns a
// closer via *codex.Agent.Close — pass that here).
func NewInProcess(
	ws *code.Workspace,
	name string,
	serverAgent acpsdk.Agent,
	setupServer func(*acpsdk.AgentSideConnection),
	cleanup func() error,
) (*Agent, error) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	a := &Agent{
		workspace: ws,
		def:       code.AgentDef{Name: name},
		stdin:     clientW, // closing this signals EOF to the server side
		sessions:  map[string]*sessionState{},
		cleanup:   cleanup,
	}

	srvConn := acpsdk.NewAgentSideConnection(serverAgent, serverW, serverR)
	if setupServer != nil {
		setupServer(srvConn)
	}
	a.serverDone = srvConn.Done()

	a.conn = acpsdk.NewClientSideConnection(a, clientW, clientR)
	a.conn.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))

	initCtx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	resp, err := a.conn.Initialize(initCtx, acpsdk.InitializeRequest{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		ClientCapabilities: acpsdk.ClientCapabilities{
			Fs: acpsdk.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
	})
	if err != nil {
		a.shutdown()
		return nil, fmt.Errorf("agent %q: initialize: %w", name, err)
	}
	a.caps = resp.AgentCapabilities
	return a, nil
}

func (a *Agent) Name() string               { return a.def.Name }
func (a *Agent) Workspace() *code.Workspace { return a.workspace }

// ─── Models / Effort (cached from ACP responses) ─────────────────

func (a *Agent) Models() ([]code.Model, string) {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	out := make([]code.Model, len(a.models))
	copy(out, a.models)
	return out, a.modelID
}

func (a *Agent) SetModel(ctx context.Context, id string) error {
	sess := a.anySession()
	if sess == nil {
		a.configMu.Lock()
		a.modelID = id
		a.configMu.Unlock()
		return nil
	}
	resp, err := a.conn.SetSessionConfigOption(ctx, acpsdk.SetSessionConfigOptionRequest{
		ValueId: &acpsdk.SetSessionConfigOptionValueId{
			SessionId: sess.id,
			ConfigId:  modelConfigID,
			Value:     acpsdk.SessionConfigValueId(id),
		},
	})
	if err != nil {
		return err
	}
	a.refreshConfig(resp.ConfigOptions)
	return nil
}

func (a *Agent) Effort() (string, []string) {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	opts := make([]string, len(a.effortOpts))
	copy(opts, a.effortOpts)
	return a.effortID, opts
}

func (a *Agent) SetEffort(ctx context.Context, value string) error {
	sess := a.anySession()
	if sess == nil {
		a.configMu.Lock()
		a.effortID = value
		a.configMu.Unlock()
		return nil
	}
	resp, err := a.conn.SetSessionConfigOption(ctx, acpsdk.SetSessionConfigOptionRequest{
		ValueId: &acpsdk.SetSessionConfigOptionValueId{
			SessionId: sess.id,
			ConfigId:  effortConfigID,
			Value:     acpsdk.SessionConfigValueId(value),
		},
	})
	if err != nil {
		return err
	}
	a.refreshConfig(resp.ConfigOptions)
	return nil
}

func (a *Agent) anySession() *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		return s
	}
	return nil
}

// ─── Modes (cached from ACP SessionModeState) ────────────────────

func (a *Agent) Modes(sessionID string) ([]code.Mode, string) {
	sess := a.session(sessionID)
	if sess == nil {
		return nil, ""
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	out := make([]code.Mode, len(sess.modes))
	copy(out, sess.modes)
	return out, sess.modeID
}

func (a *Agent) SetMode(ctx context.Context, sessionID, modeID string) error {
	sess := a.session(sessionID)
	if sess == nil {
		return errors.ErrUnsupported
	}
	sess.mu.Lock()
	hasModes := len(sess.modes) > 0
	sess.mu.Unlock()
	if !hasModes {
		return errors.ErrUnsupported
	}
	if _, err := a.conn.SetSessionMode(ctx, acpsdk.SetSessionModeRequest{
		SessionId: sess.id,
		ModeId:    acpsdk.SessionModeId(modeID),
	}); err != nil {
		return err
	}
	sess.mu.Lock()
	sess.modeID = modeID
	sess.mu.Unlock()
	return nil
}

// applyModes stores the per-session mode catalog from a session/new or
// session/load response. A nil state leaves the existing modes untouched.
func (s *sessionState) applyModes(modes *acpsdk.SessionModeState) {
	if modes == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modes = s.modes[:0]
	for _, m := range modes.AvailableModes {
		mode := code.Mode{ID: string(m.Id), Name: m.Name}
		if m.Description != nil {
			mode.Description = *m.Description
		}
		s.modes = append(s.modes, mode)
	}
	s.modeID = string(modes.CurrentModeId)
}

// ─── Sessions ─────────────────────────────────────────────────────

func (a *Agent) ListSessions(ctx context.Context) ([]code.SessionInfo, error) {
	if a.caps.SessionCapabilities.List == nil {
		return nil, nil
	}
	cwd := a.workspace.RootPath
	resp, err := a.conn.ListSessions(ctx, acpsdk.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		return nil, err
	}
	out := make([]code.SessionInfo, 0, len(resp.Sessions))
	for _, s := range resp.Sessions {
		info := code.SessionInfo{ID: string(s.SessionId)}
		if s.Title != nil {
			info.Title = *s.Title
		}
		if s.UpdatedAt != nil {
			if t, err := time.Parse(time.RFC3339, *s.UpdatedAt); err == nil {
				info.UpdatedAt = t
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func (a *Agent) NewSession(ctx context.Context) (string, error) {
	resp, err := a.conn.NewSession(ctx, acpsdk.NewSessionRequest{
		Cwd:        a.workspace.RootPath,
		McpServers: []acpsdk.McpServer{},
	})
	if err != nil {
		return "", err
	}
	a.refreshConfig(resp.ConfigOptions)

	id := string(resp.SessionId)
	sess := &sessionState{
		parent:    a,
		id:        resp.SessionId,
		toolCalls: map[string]toolCall{},
	}
	sess.applyModes(resp.Modes)
	a.mu.Lock()
	a.sessions[id] = sess
	a.mu.Unlock()
	return id, nil
}

// LoadSession drains replay synchronously: Messages(id) reflects the
// loaded transcript by the time it returns nil.
func (a *Agent) LoadSession(ctx context.Context, id string) error {
	if !a.caps.LoadSession {
		return errors.ErrUnsupported
	}
	a.mu.Lock()
	sess, exists := a.sessions[id]
	if !exists {
		sess = &sessionState{
			parent:    a,
			id:        acpsdk.SessionId(id),
			toolCalls: map[string]toolCall{},
		}
		a.sessions[id] = sess
	}
	a.mu.Unlock()

	loadCtx, cancel := context.WithCancel(ctx)
	t := &turn{
		events: make(chan event, 256),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	sess.mu.Lock()
	if sess.inflight != nil {
		sess.mu.Unlock()
		cancel()
		return fmt.Errorf("session %s is busy", id)
	}
	sess.inflight = t
	sess.mu.Unlock()

	defer func() {
		close(t.done)
		cancel()
		sess.mu.Lock()
		sess.inflight = nil
		if len(t.emitted) > 0 {
			sess.messages = append(sess.messages, t.emitted...)
		}
		sess.mu.Unlock()
	}()

	loadErrCh := make(chan error, 1)
	go func() {
		resp, err := a.conn.LoadSession(loadCtx, acpsdk.LoadSessionRequest{
			SessionId:  acpsdk.SessionId(id),
			Cwd:        a.workspace.RootPath,
			McpServers: []acpsdk.McpServer{},
		})
		if err == nil {
			a.refreshConfig(resp.ConfigOptions)
			sess.applyModes(resp.Modes)
		}
		loadErrCh <- err
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-loadErrCh:
			return err
		case ev := <-t.events:
			if ev.done {
				return ev.err
			}
			// Drained — already in t.emitted via translateUpdate.
		}
	}
}

func (a *Agent) SupportsDelete() bool {
	return a.caps.SessionCapabilities.Delete != nil
}

func (a *Agent) DeleteSession(ctx context.Context, id string) error {
	if !a.SupportsDelete() {
		return errors.ErrUnsupported
	}
	a.mu.Lock()
	delete(a.sessions, id)
	a.mu.Unlock()
	_, err := a.conn.UnstableDeleteSession(ctx, acpsdk.UnstableDeleteSessionRequest{
		SessionId: acpsdk.SessionId(id),
	})
	return err
}

func (a *Agent) Messages(id string) []agent.Message {
	sess := a.session(id)
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	out := make([]agent.Message, len(sess.messages))
	copy(out, sess.messages)
	return out
}

func (a *Agent) Usage(id string) agent.Usage {
	sess := a.session(id)
	if sess == nil {
		return agent.Usage{}
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.usage
}

func (a *Agent) session(id string) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

// ─── Send / Cancel ────────────────────────────────────────────────

func (a *Agent) Send(ctx context.Context, id string, input []agent.Content) iter.Seq2[agent.Message, error] {
	a.mu.Lock()
	sess, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok {
		return errStream(fmt.Errorf("session %s not found; call NewSession first", id))
	}

	sendCtx, cancel := context.WithCancel(ctx)
	t := &turn{
		events: make(chan event, 256),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	sess.mu.Lock()
	if sess.inflight != nil {
		sess.mu.Unlock()
		cancel()
		return nil
	}
	sess.inflight = t
	sess.messages = append(sess.messages, agent.Message{
		Role:    agent.RoleUser,
		Content: input,
	})
	sess.mu.Unlock()

	go func() {
		resp, err := a.conn.Prompt(sendCtx, acpsdk.PromptRequest{
			SessionId: sess.id,
			Prompt:    contentToBlocks(input),
		})
		// The PromptResponse carries the turn's authoritative token breakdown;
		// the per-chunk usage_update notifications can't express it (they only
		// have a total + context size), so commit it here for Usage(id).
		if err == nil && resp.Usage != nil {
			sess.mu.Lock()
			sess.usage = agent.Usage{
				InputTokens:  int64(resp.Usage.InputTokens),
				OutputTokens: int64(resp.Usage.OutputTokens),
			}
			if resp.Usage.CachedReadTokens != nil {
				sess.usage.CachedTokens = int64(*resp.Usage.CachedReadTokens)
			}
			sess.mu.Unlock()
		}
		select {
		case t.events <- event{done: true, err: err}:
		case <-t.done:
		}
	}()

	return func(yield func(agent.Message, error) bool) {
		defer func() {
			cancel()
			close(t.done)
			sess.mu.Lock()
			sess.inflight = nil
			if len(t.emitted) > 0 {
				sess.messages = append(sess.messages, t.emitted...)
			}
			sess.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				_ = a.conn.Cancel(context.Background(), acpsdk.CancelNotification{
					SessionId: sess.id,
				})
				yield(agent.Message{}, ctx.Err())
				return
			case ev := <-t.events:
				if ev.done {
					if ev.err != nil {
						yield(agent.Message{}, ev.err)
					}
					return
				}
				if !yield(ev.msg, nil) {
					return
				}
			}
		}
	}
}

func (a *Agent) Cancel(id string) {
	sess := a.session(id)
	if sess == nil {
		return
	}
	sess.mu.Lock()
	t := sess.inflight
	sess.mu.Unlock()
	if t != nil {
		t.cancel()
	}
}

func errStream(err error) iter.Seq2[agent.Message, error] {
	return func(yield func(agent.Message, error) bool) {
		yield(agent.Message{}, err)
	}
}

// ─── Lifecycle ────────────────────────────────────────────────────

func (a *Agent) Close() error {
	a.shutdown()
	return nil
}

func (a *Agent) shutdown() {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		sessions := make([]*sessionState, 0, len(a.sessions))
		for _, sess := range a.sessions {
			sessions = append(sessions, sess)
			sess.mu.Lock()
			if sess.inflight != nil {
				sess.inflight.cancel()
			}
			sess.mu.Unlock()
		}
		a.mu.Unlock()

		// Protocol: clients MUST NOT call session/close without the cap.
		if a.caps.SessionCapabilities.Close != nil && len(sessions) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			for _, sess := range sessions {
				_, _ = a.conn.CloseSession(ctx, acpsdk.CloseSessionRequest{SessionId: sess.id})
			}
			cancel()
		}

		if a.stdin != nil {
			_ = a.stdin.Close()
		}

		// In-process path: wait briefly for the server side to drain, then
		// run the caller's cleanup (e.g. codex.Spawn shutdown).
		if a.serverDone != nil {
			select {
			case <-a.serverDone:
			case <-time.After(2 * time.Second):
			}
		}
		if a.cleanup != nil {
			_ = a.cleanup()
		}

		if a.cmd == nil || a.cmd.Process == nil {
			return
		}
		exited := make(chan struct{})
		go func() {
			_ = a.cmd.Wait()
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			_ = a.cmd.Process.Kill()
			<-exited
		}
	})
}

// ─── ACP Client interface ─────────────────────────────────────────

func (a *Agent) SessionUpdate(_ context.Context, n acpsdk.SessionNotification) error {
	if n.Update.ConfigOptionUpdate != nil {
		a.refreshConfig(n.Update.ConfigOptionUpdate.ConfigOptions)
		return nil
	}

	if n.Update.CurrentModeUpdate != nil {
		if sess := a.session(string(n.SessionId)); sess != nil {
			sess.mu.Lock()
			sess.modeID = string(n.Update.CurrentModeUpdate.CurrentModeId)
			sess.mu.Unlock()
		}
		return nil
	}

	sess := a.session(string(n.SessionId))
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	t := sess.inflight
	sess.mu.Unlock()
	if t == nil {
		return nil
	}
	msg, ok := a.translateUpdate(sess, t, n.Update)
	if !ok {
		return nil
	}
	select {
	case t.events <- event{msg: msg}:
	case <-t.done:
	}
	return nil
}

func (a *Agent) translateUpdate(sess *sessionState, t *turn, u acpsdk.SessionUpdate) (agent.Message, bool) {
	emit := func(role agent.MessageRole, c agent.Content) agent.Message {
		t.mu.Lock()
		if n := len(t.emitted); n > 0 && t.emitted[n-1].Role == role {
			t.emitted[n-1].Content = append(t.emitted[n-1].Content, c)
		} else {
			t.emitted = append(t.emitted, agent.Message{Role: role, Content: []agent.Content{c}})
		}
		t.mu.Unlock()
		return agent.Message{Role: role, Content: []agent.Content{c}}
	}

	switch {
	case u.UserMessageChunk != nil:
		text := blockText(u.UserMessageChunk.Content)
		if text == "" {
			return agent.Message{}, false
		}
		return emit(agent.RoleUser, agent.Content{Text: text}), true

	case u.AgentMessageChunk != nil:
		text := blockText(u.AgentMessageChunk.Content)
		if text == "" {
			return agent.Message{}, false
		}
		return emit(agent.RoleAssistant, agent.Content{Text: text}), true

	case u.AgentThoughtChunk != nil:
		text := blockText(u.AgentThoughtChunk.Content)
		if text == "" {
			return agent.Message{}, false
		}
		id := ""
		if u.AgentThoughtChunk.MessageId != nil {
			id = *u.AgentThoughtChunk.MessageId
		}
		return emit(agent.RoleAssistant, agent.Content{Reasoning: &agent.Reasoning{ID: id, Summary: text}}), true

	case u.ToolCall != nil:
		tc := u.ToolCall
		args := rawValueToString(tc.RawInput)
		// Prefer the descriptive title (e.g. "Bash", "mcp.wingman.web_search",
		// "Read file '...'") over the generic kind ("execute"/"read"/"edit").
		name := tc.Title
		if name == "" {
			name = string(tc.Kind)
		}
		sess.toolCallsMu.Lock()
		sess.toolCalls[string(tc.ToolCallId)] = toolCall{name: name, args: args}
		sess.toolCallsMu.Unlock()
		return emit(agent.RoleAssistant, agent.Content{ToolCall: &agent.ToolCall{
			ID:   string(tc.ToolCallId),
			Name: name,
			Args: args,
		}}), true

	case u.ToolCallUpdate != nil:
		tu := u.ToolCallUpdate
		if tu.Status == nil {
			return agent.Message{}, false
		}
		status := *tu.Status
		if status != acpsdk.ToolCallStatusCompleted && status != acpsdk.ToolCallStatusFailed {
			return agent.Message{}, false
		}
		sess.toolCallsMu.Lock()
		prior := sess.toolCalls[string(tu.ToolCallId)]
		sess.toolCallsMu.Unlock()
		body := toolCallContentText(tu.Content)
		if body == "" && tu.RawOutput != nil {
			body = rawValueToString(tu.RawOutput)
		}
		if body == "" && status == acpsdk.ToolCallStatusFailed {
			body = "tool call failed"
		}
		return emit(agent.RoleAssistant, agent.Content{ToolResult: &agent.ToolResult{
			ID:      string(tu.ToolCallId),
			Name:    prior.name,
			Args:    prior.args,
			Content: body,
		}}), true

	case u.UsageUpdate != nil:
		sess.mu.Lock()
		sess.usage.InputTokens = int64(u.UsageUpdate.Used)
		sess.mu.Unlock()
	}
	return agent.Message{}, false
}

// SetUI installs the UI used to approve permission requests. Set before turns.
func (a *Agent) SetUI(ui code.UI) { a.ui = ui }

func (a *Agent) RequestPermission(ctx context.Context, p acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	cancelled := acpsdk.RequestPermissionResponse{
		Outcome: acpsdk.RequestPermissionOutcome{Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{}},
	}
	if len(p.Options) == 0 {
		return cancelled, nil
	}
	selected := func(id acpsdk.PermissionOptionId) acpsdk.RequestPermissionResponse {
		return acpsdk.RequestPermissionResponse{
			Outcome: acpsdk.RequestPermissionOutcome{
				Selected: &acpsdk.RequestPermissionOutcomeSelected{OptionId: id},
			},
		}
	}

	// No UI wired (e.g. headless): approve, preserving prior behavior.
	if a.ui == nil {
		return selected(p.Options[0].OptionId), nil
	}

	ok, err := a.ui.Confirm(code.WithSessionID(ctx, string(p.SessionId)), permissionMessage(p))
	if err != nil {
		return cancelled, nil
	}
	if ok {
		if opt := pickPermissionOption(p.Options, true); opt != nil {
			return selected(opt.OptionId), nil
		}
		return selected(p.Options[0].OptionId), nil
	}
	if opt := pickPermissionOption(p.Options, false); opt != nil {
		return selected(opt.OptionId), nil
	}
	return cancelled, nil
}

// pickPermissionOption picks by intent: allow → allow-once/always, deny →
// reject-once/always. Returns nil when no option matches.
func pickPermissionOption(opts []acpsdk.PermissionOption, allow bool) *acpsdk.PermissionOption {
	want := []acpsdk.PermissionOptionKind{acpsdk.PermissionOptionKindRejectOnce, acpsdk.PermissionOptionKindRejectAlways}
	if allow {
		want = []acpsdk.PermissionOptionKind{acpsdk.PermissionOptionKindAllowOnce, acpsdk.PermissionOptionKindAllowAlways}
	}
	for _, k := range want {
		for i := range opts {
			if opts[i].Kind == k {
				return &opts[i]
			}
		}
	}
	return nil
}

// permissionMessage renders a confirm prompt from the tool title, command and reason.
func permissionMessage(p acpsdk.RequestPermissionRequest) string {
	var parts []string
	if p.ToolCall.Title != nil && *p.ToolCall.Title != "" {
		parts = append(parts, *p.ToolCall.Title)
	}
	if m, ok := p.ToolCall.RawInput.(map[string]any); ok {
		if cmd, ok := m["command"].(string); ok && cmd != "" {
			parts = append(parts, "$ "+cmd)
		}
	}
	if t := toolCallContentText(p.ToolCall.Content); t != "" {
		parts = append(parts, t)
	}
	if len(parts) == 0 {
		return "Allow this action?"
	}
	return strings.Join(parts, "\n\n")
}

func (a *Agent) WriteTextFile(_ context.Context, p acpsdk.WriteTextFileRequest) (acpsdk.WriteTextFileResponse, error) {
	abs, err := a.resolvePath(p.Path)
	if err != nil {
		return acpsdk.WriteTextFileResponse{}, err
	}
	_ = os.MkdirAll(filepath.Dir(abs), 0o755)
	return acpsdk.WriteTextFileResponse{}, os.WriteFile(abs, []byte(p.Content), 0o644)
}

func (a *Agent) ReadTextFile(_ context.Context, p acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	abs, err := a.resolvePath(p.Path)
	if err != nil {
		return acpsdk.ReadTextFileResponse{}, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return acpsdk.ReadTextFileResponse{}, err
	}
	content := string(data)
	if p.Line != nil || p.Limit != nil {
		lines := strings.Split(content, "\n")
		start := 0
		if p.Line != nil && *p.Line > 0 {
			start = *p.Line - 1
			if start > len(lines) {
				start = len(lines)
			}
		}
		end := len(lines)
		if p.Limit != nil && *p.Limit > 0 && start+*p.Limit < end {
			end = start + *p.Limit
		}
		content = strings.Join(lines[start:end], "\n")
	}
	return acpsdk.ReadTextFileResponse{Content: content}, nil
}

func (a *Agent) resolvePath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute: %s", p)
	}
	clean := filepath.Clean(p)
	root := filepath.Clean(a.workspace.RootPath)
	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return "", fmt.Errorf("path outside workspace: %s", p)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path outside workspace: %s", p)
	}
	return clean, nil
}

func (a *Agent) CreateTerminal(context.Context, acpsdk.CreateTerminalRequest) (acpsdk.CreateTerminalResponse, error) {
	return acpsdk.CreateTerminalResponse{}, errors.New("terminal not supported")
}
func (a *Agent) KillTerminal(context.Context, acpsdk.KillTerminalRequest) (acpsdk.KillTerminalResponse, error) {
	return acpsdk.KillTerminalResponse{}, nil
}
func (a *Agent) ReleaseTerminal(context.Context, acpsdk.ReleaseTerminalRequest) (acpsdk.ReleaseTerminalResponse, error) {
	return acpsdk.ReleaseTerminalResponse{}, nil
}
func (a *Agent) TerminalOutput(context.Context, acpsdk.TerminalOutputRequest) (acpsdk.TerminalOutputResponse, error) {
	return acpsdk.TerminalOutputResponse{Output: ""}, nil
}
func (a *Agent) WaitForTerminalExit(context.Context, acpsdk.WaitForTerminalExitRequest) (acpsdk.WaitForTerminalExitResponse, error) {
	return acpsdk.WaitForTerminalExitResponse{}, nil
}

// ─── Config conversion ────────────────────────────────────────────

// refreshConfig updates the connection-global model + effort catalog from the
// session's config options. Modes are per-session — see [sessionState.applyModes].
func (a *Agent) refreshConfig(options []acpsdk.SessionConfigOption) {
	if options == nil {
		return
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	a.effortID = ""
	a.effortOpts = nil
	for _, opt := range options {
		if opt.Select == nil {
			continue
		}
		switch string(opt.Select.Id) {
		case modelConfigID:
			a.models = a.models[:0]
			if u := opt.Select.Options.Ungrouped; u != nil {
				for _, v := range *u {
					a.models = append(a.models, code.Model{ID: string(v.Value), Name: v.Name})
				}
			}
			a.modelID = string(opt.Select.CurrentValue)
		case effortConfigID:
			a.effortID = string(opt.Select.CurrentValue)
			if u := opt.Select.Options.Ungrouped; u != nil {
				for _, v := range *u {
					a.effortOpts = append(a.effortOpts, string(v.Value))
				}
			}
		}
	}
}

// ─── Content helpers ──────────────────────────────────────────────

func contentToBlocks(input []agent.Content) []acpsdk.ContentBlock {
	out := make([]acpsdk.ContentBlock, 0, len(input))
	for _, c := range input {
		switch {
		case c.Text != "":
			out = append(out, acpsdk.TextBlock(c.Text))
		case c.File != nil && c.File.Data != "":
			if mime, data, ok := splitDataURL(c.File.Data); ok {
				out = append(out, acpsdk.ImageBlock(data, mime))
			}
		}
	}
	return out
}

func blockText(b acpsdk.ContentBlock) string {
	if b.Text != nil {
		return b.Text.Text
	}
	return ""
}

func toolCallContentText(items []acpsdk.ToolCallContent) string {
	var parts []string
	for _, item := range items {
		switch {
		case item.Content != nil:
			if t := blockText(item.Content.Content); t != "" {
				parts = append(parts, t)
			}
		case item.Diff != nil:
			// File edits arrive as diff blocks (old/new text). Render them as
			// a unified-style diff so the change is visible in the tool output
			// instead of showing nothing.
			if t := diffBlockText(item.Diff); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// diffBlockText renders an ACP diff content block as a line-prefixed diff
// (" " context, "-" removed, "+" added), preceded by the file path.
func diffBlockText(d *acpsdk.ToolCallContentDiff) string {
	old := ""
	if d.OldText != nil {
		old = *d.OldText
	}

	dmp := diffmatchpatch.New()
	c1, c2, lines := dmp.DiffLinesToChars(old, d.NewText)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(c1, c2, false), lines)

	var b strings.Builder
	if d.Path != "" {
		b.WriteString(d.Path)
		b.WriteByte('\n')
	}
	for _, df := range diffs {
		prefix := " "
		switch df.Type {
		case diffmatchpatch.DiffInsert:
			prefix = "+"
		case diffmatchpatch.DiffDelete:
			prefix = "-"
		}
		for _, ln := range strings.Split(strings.TrimSuffix(df.Text, "\n"), "\n") {
			b.WriteString(prefix)
			b.WriteString(ln)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func rawValueToString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if data, err := json.Marshal(v); err == nil {
		return string(data)
	}
	return fmt.Sprintf("%v", v)
}

func splitDataURL(s string) (mime, data string, ok bool) {
	rest, found := strings.CutPrefix(s, "data:")
	if !found {
		return "", "", false
	}
	mime, data, ok = strings.Cut(rest, ";base64,")
	return mime, data, ok
}
