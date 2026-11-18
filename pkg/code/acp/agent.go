package acp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/adrianliechti/wingman-agent/internal/acpjson"
	"github.com/adrianliechti/wingman-agent/internal/process"
	"github.com/adrianliechti/wingman-agent/pkg/acp"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/model"
)

type Agent struct {
	workspace *code.Workspace
	def       code.AgentDef
	// login is what the agent asks of a user who has not logged in yet, by
	// the method loginMethod, and title how it names itself.
	login       string
	loginMethod string
	title       string

	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        io.Closer
	conn          agentConnection
	closeOnce     sync.Once
	transportOnce sync.Once
	processDone   chan struct{}

	serverDone <-chan struct{}
	serverW    io.Closer
	cleanup    func() error
	steer      func(context.Context, acpsdk.SessionId, []acpsdk.ContentBlock, string) error

	caps acpsdk.AgentCapabilities

	uiMu          sync.RWMutex
	ui            code.UI
	updateMu      sync.RWMutex
	updateHandler func(string)
	updatePending map[string]bool
	updateRunning bool

	configMu      sync.RWMutex
	models        []model.Model
	modelID       string
	modelDefault  string
	effortID      string
	effortDefault string
	effortOpts    []string

	mu       sync.Mutex
	sessions map[string]*sessionState
	pending  map[string][]acpsdk.SessionUpdate
}

var (
	_ code.Agent                   = (*Agent)(nil)
	_ code.HistorySnapshotProvider = (*Agent)(nil)
	_ code.HistoryVersionProvider  = (*Agent)(nil)
)

type sessionState struct {
	id acpsdk.SessionId
	// Installed before publication when notices arrived during session setup.
	initialNotices func()

	mu         sync.Mutex
	messages   []harness.Message
	v2Messages map[string]int
	usage      harness.Usage
	inflight   *turn
	loaded     bool

	toolCallsMu sync.Mutex
	toolCalls   map[string]toolCall

	modeOptID string
	modes     []code.Mode
	modeID    string

	models      []model.Model
	modelID     string
	modelOptID  string
	effortID    string
	effortOptID string
	effortOpts  []string

	commands  []code.Command
	title     string
	updatedAt time.Time
}

type toolCall struct {
	name      string
	kind      string
	args      string
	locations []harness.ToolLocation
	content   tool.Content
	rawOutput string
}

type turn struct {
	ctx      context.Context
	events   chan *event
	done     chan struct{}
	cancel   context.CancelFunc
	updateMu sync.Mutex
	// A history load reads snapshots of emitted, so its events only say that something changed.
	replay bool

	mu                sync.Mutex
	emitted           []harness.Message
	v2Messages        map[string]int
	lastContentKey    string
	ignoreUserUpdates bool
	streamErr         error
}

// send drops the message once the turn is finished.
func (t *turn) send(msg harness.Message) {
	select {
	case <-t.done:
		return
	case <-t.ctx.Done():
		return
	default:
	}
	select {
	case t.events <- &event{msg: msg}:
	case <-t.done:
	case <-t.ctx.Done():
	default:
		if t.replay {
			return
		}
		t.mu.Lock()
		t.streamErr = errors.New("ACP session update buffer exhausted; the consumer stopped reading")
		t.mu.Unlock()
		t.cancel()
	}
}

// messages holds t.mu only for the last message, the one emit mutates, so long histories don't stall SessionUpdate.
func (t *turn) messages() []harness.Message {
	t.mu.Lock()
	if t.v2Messages != nil {
		messages := harness.CloneMessages(t.emitted)
		t.mu.Unlock()
		return messages
	}
	n := len(t.emitted)
	if n == 0 {
		t.mu.Unlock()
		return nil
	}
	prefix := t.emitted[: n-1 : n-1]
	last := harness.CloneMessages(t.emitted[n-1:])
	t.mu.Unlock()
	return append(harness.CloneMessages(prefix), last...)
}

// cancelOutstandingToolCalls renders tool calls that never reached a terminal status.
func (s *sessionState) cancelOutstandingToolCalls(t *turn) []harness.Message {
	s.toolCallsMu.Lock()
	pending := s.toolCalls
	s.toolCalls = map[string]toolCall{}
	s.toolCallsMu.Unlock()

	messages := make([]harness.Message, 0, len(pending))
	for _, id := range slices.Sorted(maps.Keys(pending)) {
		call := pending[id]
		msg := harness.Message{Role: harness.RoleAssistant, Content: []harness.Content{{ToolResult: &harness.ToolResult{
			ID:        id,
			Name:      call.name,
			Kind:      call.kind,
			Args:      call.args,
			Locations: call.locations,
			Presentation: harness.NewToolPresentation(
				call.name, call.kind, call.args, call.locations,
			),
			Content: tool.TextContent("tool call cancelled"),
			IsError: true,
		}}}}
		t.mu.Lock()
		t.emitted = append(t.emitted, msg)
		t.lastContentKey = ""
		t.mu.Unlock()
		messages = append(messages, msg)
	}
	return messages
}

func (s *sessionState) finalizeTurn(t *turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight == t {
		s.inflight = nil
	}
	s.retainTurn(t)
}

// Caller holds s.mu. Keep entity indexes when a replay or foreground response
// becomes retained history so later v2 updates can still address its messages.
func (s *sessionState) retainTurn(t *turn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.v2Messages) > 0 {
		if s.v2Messages == nil {
			s.v2Messages = map[string]int{}
		}
		for key, idx := range t.v2Messages {
			s.v2Messages[key] = len(s.messages) + idx
		}
	}
	s.messages = append(s.messages, harness.CloneMessages(t.emitted)...)

}

type event struct {
	msg  harness.Message
	err  error
	done bool
}

// How far a live turn's consumer may fall behind before the turn fails; an unread turn must not grow without bound.
const turnEventBacklog = 1 << 14

const (
	modelConfigID  = "model"
	effortConfigID = "effort"
	initTimeout    = 30 * time.Second
)

func New(ctx context.Context, ws *code.Workspace, def code.AgentDef) (*Agent, error) {
	if def.Command == "" {
		return nil, fmt.Errorf("agent %q: empty command", def.Name)
	}

	cwd := ws.RootPath
	cmd := exec.Command(def.Command, def.Args...)
	cmd.WaitDelay = 2 * time.Second
	process.Hide(cmd)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for k, v := range def.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("agent %q: stdin pipe: %w", def.Name, err)
	}
	// Own stdout so Wait can observe process death while the reader drains
	// final frames, even when a subprocess inherited the output pipe.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("agent %q: stdout pipe: %w", def.Name, err)
	}
	cmd.Stdout = stdoutWriter
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return nil, fmt.Errorf("agent %q: start: %w", def.Name, err)
	}
	_ = stdoutWriter.Close()

	a := &Agent{
		workspace: ws,
		def:       def,
		cmd:       cmd,
		stdin:     acp.NewConnectionWriter(stdin, 0),
		stdout:    stdout,
		sessions:  map[string]*sessionState{},
		pending:   map[string][]acpsdk.SessionUpdate{},
	}
	a.processDone = make(chan struct{})
	connected := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(a.processDone)
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-connected:
			if a.conn == nil {
				return
			}
			select {
			case <-a.conn.Done():
				return
			case <-timer.C:
			}
		case <-timer.C:
		}
		a.abortTransport()
	}()
	err = a.connectExternal(ctx, stdout)
	close(connected)
	if err != nil {
		a.shutdown()
		return nil, fmt.Errorf("agent %q: initialize: %w", def.Name, err)
	}
	a.watchWriter(a.stdin.(*acp.ConnectionWriter))
	a.conn.SetLogger(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return a, nil
}

func NewInProcess(
	ctx context.Context,
	ws *code.Workspace,
	def code.AgentDef,
	serverAgent acpsdk.Agent,
	setupServer func(*acpsdk.AgentSideConnection),
	cleanup func() error,
) (*Agent, error) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	a := &Agent{
		workspace: ws,
		def:       def,
		stdin:     acp.NewConnectionWriter(clientW, 0),
		stdout:    clientR,
		sessions:  map[string]*sessionState{},
		pending:   map[string][]acpsdk.SessionUpdate{},
		cleanup:   cleanup,
	}
	if s, ok := serverAgent.(interface {
		Steer(context.Context, acpsdk.SessionId, []acpsdk.ContentBlock, string) error
	}); ok {
		a.steer = s.Steer
	}

	serverWriter := acp.NewConnectionWriter(serverW, 0)
	srvConn := acpsdk.NewAgentSideConnection(serverAgent, serverWriter, serverR)
	if setupServer != nil {
		setupServer(srvConn)
	}
	a.serverDone = srvConn.Done()
	a.serverW = serverWriter

	v1 := acpsdk.NewClientSideConnection(a, a.stdin, clientR)
	a.conn = v1
	a.watchWriter(a.stdin.(*acp.ConnectionWriter))
	a.watchWriter(serverWriter)
	a.conn.SetLogger(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))

	resp, err := a.initialize(ctx, v1)
	if err != nil {
		a.shutdown()
		return nil, fmt.Errorf("agent %q: initialize: %w", def.Name, err)
	}
	a.caps = resp.AgentCapabilities
	a.login, a.loginMethod = loginHint(resp.AuthMethods)
	a.title = agentTitle(resp.AgentInfo)
	return a, nil
}

func (a *Agent) initialize(ctx context.Context, conn *acpsdk.ClientSideConnection) (acpsdk.InitializeResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	resp, err := acp.Call(initCtx, func() (acpsdk.InitializeResponse, error) { return conn.Initialize(initCtx, hostInitialize()) })
	if err != nil {
		return acpsdk.InitializeResponse{}, err
	}
	// Major protocol versions can contain breaking changes. Accept only a
	// version this implementation actually supports.
	if resp.ProtocolVersion != acpsdk.ProtocolVersionNumber {
		return acpsdk.InitializeResponse{}, fmt.Errorf("unsupported ACP protocol version %d (want %d)", resp.ProtocolVersion, acpsdk.ProtocolVersionNumber)
	}
	return resp, nil
}

func (a *Agent) Name() string               { return a.def.Name }
func (a *Agent) Workspace() *code.Workspace { return a.workspace }

func (a *Agent) Models(sessionID string) ([]model.Model, string) {
	if sess := a.session(sessionID); sess != nil {
		sess.mu.Lock()
		out := append([]model.Model(nil), sess.models...)
		current := sess.modelID
		sess.mu.Unlock()
		return out, current
	}
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	out := make([]model.Model, len(a.models))
	copy(out, a.models)
	if a.modelDefault != "" {
		return out, a.modelDefault
	}
	return out, a.modelID
}

func (a *Agent) SetModel(ctx context.Context, sessionID, id string) error {
	if sess := a.session(sessionID); sess != nil {
		sess.mu.Lock()
		optionID := sess.modelOptID
		sess.mu.Unlock()
		return a.setSessionConfig(ctx, sess, optionID, id)
	}
	a.configMu.Lock()
	a.modelDefault = id
	a.configMu.Unlock()
	return nil
}

func (a *Agent) Effort(sessionID string) (string, []string) {
	if sess := a.session(sessionID); sess != nil {
		sess.mu.Lock()
		current := sess.effortID
		opts := append([]string(nil), sess.effortOpts...)
		sess.mu.Unlock()
		return current, opts
	}
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	opts := make([]string, len(a.effortOpts))
	copy(opts, a.effortOpts)
	if a.effortDefault != "" {
		return a.effortDefault, opts
	}
	return a.effortID, opts
}

func (a *Agent) SetEffort(ctx context.Context, sessionID, value string) error {
	if sess := a.session(sessionID); sess != nil {
		sess.mu.Lock()
		optionID := sess.effortOptID
		sess.mu.Unlock()
		return a.setSessionConfig(ctx, sess, optionID, value)
	}
	a.configMu.Lock()
	a.effortDefault = value
	a.configMu.Unlock()
	return nil
}

func (a *Agent) setSessionConfig(ctx context.Context, sess *sessionState, configID, value string) error {
	if configID == "" {
		return errors.ErrUnsupported
	}
	resp, err := a.conn.SetSessionConfigOption(ctx, acpsdk.SetSessionConfigOptionRequest{
		ValueId: &acpsdk.SetSessionConfigOptionValueId{
			SessionId: sess.id,
			ConfigId:  acpsdk.SessionConfigId(configID),
			Value:     acpsdk.SessionConfigValueId(value),
		},
	})
	if err != nil {
		return err
	}
	a.refreshConfig(sess, resp.ConfigOptions)
	return nil
}

func (a *Agent) applySessionDefaults(ctx context.Context, sess *sessionState) error {
	a.configMu.RLock()
	modelID, effortID := a.modelDefault, a.effortDefault
	a.configMu.RUnlock()

	sess.mu.Lock()
	currentModel, currentEffort := sess.modelID, sess.effortID
	modelOptID := sess.modelOptID
	sess.mu.Unlock()
	if modelID != "" && modelID != currentModel {
		if err := a.setSessionConfig(ctx, sess, modelOptID, modelID); err != nil {
			return fmt.Errorf("set default model: %w", err)
		}
	}
	if effortID != "" && effortID != currentEffort {
		sess.mu.Lock()
		effortOptID := sess.effortOptID
		sess.mu.Unlock()
		if err := a.setSessionConfig(ctx, sess, effortOptID, effortID); err != nil {
			return fmt.Errorf("set default effort: %w", err)
		}
	}
	return nil
}

func (a *Agent) Modes(sessionID string) ([]code.Mode, string) {
	sess := a.session(sessionID)
	if sess == nil {
		return nil, ""
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return append([]code.Mode(nil), sess.modes...), sess.modeID
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

// ListSessions lists the sessions of the agent's workspace.
func (a *Agent) ListSessions(ctx context.Context) ([]code.SessionInfo, error) {
	cwd := a.workspace.RootPath
	return a.listSessions(ctx, &cwd)
}

// ListsAllSessions reports whether the agent, asked without a folder, lists
// the sessions of every folder. Only agents known to do so are asked: another
// may answer with its own folder's sessions, or with none.
func (a *Agent) ListsAllSessions() bool {
	return a.def.AllFolders && a.caps.SessionCapabilities.List != nil
}

// ListAllSessions lists the sessions of every folder, each with its own.
func (a *Agent) ListAllSessions(ctx context.Context) ([]code.SessionInfo, error) {
	return a.listSessions(ctx, nil)
}

func (a *Agent) listSessions(ctx context.Context, cwd *string) ([]code.SessionInfo, error) {
	if a.caps.SessionCapabilities.List == nil {
		return nil, nil
	}
	var cursor *string
	seen := map[string]bool{}
	var out []code.SessionInfo
	for {
		resp, err := a.conn.ListSessions(ctx, acpsdk.ListSessionsRequest{Cwd: cwd, Cursor: cursor})
		if err != nil {
			return nil, a.loginError(err)
		}
		for _, s := range resp.Sessions {
			info := code.SessionInfo{ID: string(s.SessionId), Dir: s.Cwd}
			if s.Title != nil {
				info.Title = *s.Title
			}
			if s.UpdatedAt != nil {
				if t, err := time.Parse(time.RFC3339, *s.UpdatedAt); err == nil {
					info.UpdatedAt = t
				}
			}
			if local := a.session(info.ID); local != nil {
				local.mu.Lock()
				if local.title != "" {
					info.Title = local.title
				}
				if !local.updatedAt.IsZero() {
					info.UpdatedAt = local.updatedAt
				}
				local.mu.Unlock()
			}
			out = append(out, info)
		}
		if resp.NextCursor == nil || *resp.NextCursor == "" {
			return out, nil
		}
		if seen[*resp.NextCursor] {
			return nil, fmt.Errorf("agent returned repeated session cursor %q", *resp.NextCursor)
		}
		seen[*resp.NextCursor] = true
		cursor = resp.NextCursor
	}
}

func (a *Agent) NewSession(ctx context.Context) (string, error) {
	resp, err := withLogin(ctx, a, func() (acpsdk.NewSessionResponse, error) {
		return a.conn.NewSession(ctx, acpsdk.NewSessionRequest{
			Cwd:        a.workspace.RootPath,
			McpServers: []acpsdk.McpServer{},
		})
	})
	if err != nil {
		return "", err
	}
	if resp.SessionId == "" {
		return "", errors.New("ACP agent returned an empty session ID")
	}

	id := string(resp.SessionId)
	sess := &sessionState{
		id:        resp.SessionId,
		toolCalls: map[string]toolCall{},
	}
	a.refreshConfig(sess, resp.ConfigOptions)
	sess.applyModes(resp.Modes)
	sess.loaded = true
	a.mu.Lock()
	a.applyPendingLocked(id, sess)
	a.sessions[id] = sess
	a.mu.Unlock()
	if sess.initialNotices != nil {
		sess.initialNotices()
	}
	if err := a.applySessionDefaults(ctx, sess); err != nil {
		a.mu.Lock()
		delete(a.sessions, id)
		a.mu.Unlock()
		if a.caps.SessionCapabilities.Close != nil {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_, _ = a.conn.CloseSession(closeCtx, acpsdk.CloseSessionRequest{SessionId: sess.id})
			cancel()
		}
		return "", err
	}
	return id, nil
}

// authRequired is ACP's error for a user who has not logged in to the agent.
const authRequired = -32000

// loginHint is what an agent asks of a user who has not logged in, from the
// first login method it offers: its description, else its name. It has an id
// when the agent takes the login up itself through `authenticate`; one the
// user makes in a terminal is not passed to it.
func loginHint(methods []acpsdk.AuthMethod) (hint, id string) {
	for _, method := range methods {
		var name string
		var description *string
		switch {
		case method.Agent != nil:
			id, name, description = string(method.Agent.Id), method.Agent.Name, method.Agent.Description
		case method.Terminal != nil:
			name, description = method.Terminal.Name, method.Terminal.Description
		default:
			continue
		}
		if description != nil && *description != "" {
			return *description, id
		}
		return name, id
	}
	return "", ""
}

// withLogin runs a request the user asked for. An agent that wants a login
// is asked to take up the one the user made, such as with `copilot login` in
// a terminal, which it read only when it started, and the request runs once
// more.
func withLogin[T any](ctx context.Context, a *Agent, call func() (T, error)) (T, error) {
	result, err := call()
	var request *acpsdk.RequestError
	if !errors.As(err, &request) || request.Code != authRequired || a.loginMethod == "" {
		return result, a.loginError(err)
	}
	// Only a connection of the first protocol version takes a login up.
	login, ok := a.conn.(interface {
		Authenticate(context.Context, acpsdk.AuthenticateRequest) (acpsdk.AuthenticateResponse, error)
	})
	if !ok {
		return result, a.loginError(err)
	}
	if _, authErr := login.Authenticate(ctx, acpsdk.AuthenticateRequest{MethodId: acpsdk.AuthMethodId(a.loginMethod)}); authErr != nil {
		return result, a.loginError(err)
	}
	result, err = call()
	return result, a.loginError(err)
}

// agentTitle is how an agent names itself.
func agentTitle(info *acpsdk.Implementation) string {
	if info == nil {
		return ""
	}
	if info.Title != nil && *info.Title != "" {
		return *info.Title
	}
	return info.Name
}

// loginError tells a user who has not logged in to the agent how to, where
// the agent only answers "authentication required".
func (a *Agent) loginError(err error) error {
	var request *acpsdk.RequestError
	if !errors.As(err, &request) || request.Code != authRequired {
		return err
	}
	name := cmp.Or(a.title, a.def.Name, "The agent")
	if a.login == "" {
		return fmt.Errorf("%s needs you to log in: %w", name, err)
	}
	return fmt.Errorf("%s needs you to log in: %s", name, a.login)
}

// notFound tells a session the agent does not have, which it answers with
// "resource not found", from other failures to load one.
func notFound(err error, id string) error {
	var request *acpsdk.RequestError
	if errors.As(err, &request) && request.Code == acp.CodeResourceNotFound {
		return fmt.Errorf("%w: %s", code.ErrSessionNotFound, id)
	}
	return err
}

func (a *Agent) LoadSession(ctx context.Context, id string) error {
	var ferr error
	for _, err := range a.loadSessionStream(ctx, id, false) {
		if err != nil {
			ferr = err
		}
	}
	return ferr
}

func (a *Agent) LoadSessionStream(ctx context.Context, id string) iter.Seq2[[]harness.Message, error] {
	return a.loadSessionStream(ctx, id, true)
}

func (a *Agent) loadSessionStream(ctx context.Context, id string, snapshots bool) iter.Seq2[[]harness.Message, error] {
	if !a.caps.LoadSession && a.caps.SessionCapabilities.Resume == nil {
		return func(yield func([]harness.Message, error) bool) {
			yield(nil, errors.ErrUnsupported)
		}
	}

	return func(yield func([]harness.Message, error) bool) {
		a.mu.Lock()
		sess, exists := a.sessions[id]
		if !exists {
			sess = &sessionState{
				id:        acpsdk.SessionId(id),
				toolCalls: map[string]toolCall{},
			}
			a.applyPendingLocked(id, sess)
			a.sessions[id] = sess
		}
		a.mu.Unlock()

		if sess.initialNotices != nil {
			sess.initialNotices()
		}
		loadCtx, cancel := context.WithCancel(ctx)
		t := &turn{
			ctx:    loadCtx,
			events: make(chan *event, 1),
			done:   make(chan struct{}),
			cancel: cancel,
			replay: true,
		}
		sess.mu.Lock()
		if sess.loaded {
			snap := harness.CloneMessages(sess.messages)
			sess.mu.Unlock()
			cancel()
			yield(snap, nil)
			return
		}
		if sess.inflight != nil {
			sess.mu.Unlock()
			cancel()
			yield(nil, fmt.Errorf("session %s is busy", id))
			return
		}
		sess.inflight = t
		sess.mu.Unlock()

		rpcCtx, cancelRPC := context.WithCancel(context.WithoutCancel(loadCtx))
		responseDone := make(chan struct{})
		cleanupDone := a.drainTurn(t, sess.id, responseDone, cancelRPC, false)
		ok := false
		defer func() {
			close(t.done)
			cancel()
			<-cleanupDone
			sess.toolCallsMu.Lock()
			clear(sess.toolCalls)
			sess.toolCallsMu.Unlock()
			sess.mu.Lock()
			sess.inflight = nil
			if ok {
				sess.retainTurn(t)
				sess.loaded = true
			}
			sess.mu.Unlock()
			if !ok && !exists {
				a.mu.Lock()
				if a.sessions[id] == sess {
					delete(a.sessions, id)
				}
				a.mu.Unlock()
			}
		}()

		loadErrCh := make(chan error, 1)
		go func() {
			defer close(responseDone)
			defer cancelRPC()
			if a.caps.LoadSession {
				resp, err := withLogin(rpcCtx, a, func() (acpsdk.LoadSessionResponse, error) {
					return a.conn.LoadSession(rpcCtx, acpsdk.LoadSessionRequest{
						SessionId:  acpsdk.SessionId(id),
						Cwd:        a.workspace.RootPath,
						McpServers: []acpsdk.McpServer{},
					})
				})
				if err == nil {
					a.refreshConfig(sess, resp.ConfigOptions)
					sess.applyModes(resp.Modes)
				}
				loadErrCh <- notFound(err, id)
				return
			}
			resp, err := withLogin(rpcCtx, a, func() (acpsdk.ResumeSessionResponse, error) {
				return a.conn.ResumeSession(rpcCtx, acpsdk.ResumeSessionRequest{
					SessionId:  acpsdk.SessionId(id),
					Cwd:        a.workspace.RootPath,
					McpServers: []acpsdk.McpServer{},
				})
			})
			if err == nil {
				a.refreshConfig(sess, resp.ConfigOptions)
				sess.applyModes(resp.Modes)
			}
			loadErrCh <- notFound(err, id)
		}()

		for {
			select {
			case <-loadCtx.Done():
				t.mu.Lock()
				err := t.streamErr
				t.mu.Unlock()
				if err == nil {
					err = loadCtx.Err()
				}
				yield(nil, err)
				return
			case err := <-loadErrCh:
				if err != nil {
					yield(nil, err)
					return
				}
				ok = true
				yield(t.messages(), nil)
				return
			case ev := <-t.events:
				if ev.done {
					if ev.err != nil {
						yield(nil, ev.err)
						return
					}
					continue
				}
				if !snapshots {
					continue
				}
				if !yield(t.messages(), nil) {
					return
				}
			}
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
	_, err := a.conn.DeleteSession(ctx, acpsdk.DeleteSessionRequest{
		SessionId: acpsdk.SessionId(id),
	})
	if errors.Is(notFound(err, id), code.ErrSessionNotFound) {
		err = nil // Deletion is idempotent when the agent already removed it.
	}
	if err == nil {
		a.mu.Lock()
		delete(a.sessions, id)
		a.mu.Unlock()
	}
	return err
}

func (a *Agent) Messages(id string) []harness.Message {
	return readSession(a, id, func(s *sessionState) []harness.Message {
		return harness.CloneMessages(s.messages)
	})
}

func (a *Agent) HistorySnapshot(id string) code.HistorySnapshot {
	return readSession(a, id, func(s *sessionState) code.HistorySnapshot {
		return code.HistorySnapshot{Messages: harness.CloneMessages(s.messages)}
	})
}

func (a *Agent) HistoryVersion(id string) code.HistoryVersion {
	return readSession(a, id, func(s *sessionState) code.HistoryVersion {
		return code.HistoryVersion{MessageCount: len(s.messages)}
	})
}

func (a *Agent) Commands(id string) []code.Command {
	return readSession(a, id, func(s *sessionState) []code.Command {
		return append([]code.Command(nil), s.commands...)
	})
}

func (a *Agent) Usage(id string) harness.Usage {
	return readSession(a, id, func(s *sessionState) harness.Usage { return s.usage })
}

// readSession runs fn under the session lock, or returns the zero value when
// the session is unknown.
func readSession[T any](a *Agent, id string, fn func(*sessionState) T) T {
	sess := a.session(id)
	if sess == nil {
		var zero T
		return zero
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return fn(sess)
}

func (a *Agent) session(id string) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

func (a *Agent) Send(ctx context.Context, id string, input []harness.Content) (iter.Seq2[harness.Message, error], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, code.ErrEmptyInput
	}
	// The external agent reads attached context with the input.
	input = harness.ContextContent(harness.InputContextFromContext(ctx), harness.CloneContent(input))
	a.mu.Lock()
	sess, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found; call NewSession first", id)
	}

	sendCtx, cancel := context.WithCancel(ctx)
	t := &turn{
		ctx:               sendCtx,
		events:            make(chan *event, turnEventBacklog),
		done:              make(chan struct{}),
		cancel:            cancel,
		ignoreUserUpdates: true,
	}
	sess.mu.Lock()
	if sess.inflight != nil {
		sess.mu.Unlock()
		cancel()
		return nil, code.ErrTurnInProgress
	}
	sess.inflight = t
	sess.messages = append(sess.messages, harness.Message{
		Role:    harness.RoleUser,
		InputID: harness.InputIDFromContext(ctx),
		Content: input,
	})
	sess.mu.Unlock()

	// Keep the original request alive through session/cancel. Its response is
	// the fence after the final notifications for this turn.
	rpcCtx, cancelRPC := context.WithCancel(context.WithoutCancel(sendCtx))
	responseDone := make(chan struct{})
	cleanupDone := a.drainTurn(t, sess.id, responseDone, cancelRPC, true)
	go func() {
		defer cancelRPC()
		resp, err := a.conn.Prompt(rpcCtx, acpsdk.PromptRequest{
			SessionId: sess.id,
			Prompt:    acp.ContentToBlocks(input),
		})
		remoteCancelled := err == nil && resp.StopReason == acpsdk.StopReasonCancelled
		if remoteCancelled {
			err = context.Canceled
		} else if err == nil {
			if reason := stopReasonError(resp.StopReason); reason != nil {
				err = reason
			}
		}

		if resp.Usage != nil {
			sess.mu.Lock()
			lastInputTokens := sess.usage.LastInputTokens
			contextWindow := sess.usage.ContextWindow
			usage := usageFromACP(resp.Usage)
			usage.LastInputTokens = lastInputTokens
			usage.ContextWindow = contextWindow
			sess.usage = usage
			sess.mu.Unlock()
		}
		close(responseDone)
		select {
		case t.events <- &event{done: true, err: err}:
		case <-t.done:
		}
	}()

	return func(yield func(harness.Message, error) bool) {
		defer func() {
			cancel()
			close(t.done)
			<-cleanupDone
			sess.finalizeTurn(t)
		}()
		flushCancelled := func() bool {
			t.updateMu.Lock()
			defer t.updateMu.Unlock()
			for _, msg := range sess.cancelOutstandingToolCalls(t) {
				if !yield(msg, nil) {
					return false
				}
			}
			return true
		}
		for {
			select {
			case <-sendCtx.Done():
				if flushCancelled() {
					t.mu.Lock()
					err := t.streamErr
					t.mu.Unlock()
					if err == nil {
						err = sendCtx.Err()
					}
					yield(harness.Message{}, err)
				}
				return
			case ev := <-t.events:
				if ev.done {
					if !flushCancelled() {
						return
					}
					err := ev.err
					if sendCtx.Err() != nil {
						t.mu.Lock()
						err = t.streamErr
						t.mu.Unlock()
						if err == nil {
							err = sendCtx.Err()
						}
					}
					if err != nil {
						yield(harness.Message{}, err)
					}
					return
				}
				if !yield(ev.msg, nil) {
					return
				}
			}
		}
	}, nil
}

// stopReasonError reports a turn cut short; end_turn and cancelled are the caller's.
func stopReasonError(reason acpsdk.StopReason) error {
	switch reason {
	case acpsdk.StopReasonRefusal:
		return errors.New("the agent refused to continue this turn")
	case acpsdk.StopReasonMaxTokens:
		return errors.New("the agent stopped: output token limit reached")
	case acpsdk.StopReasonMaxTurnRequests:
		return errors.New("the agent stopped: maximum requests per turn reached")
	case acpsdk.StopReasonEndTurn, acpsdk.StopReasonCancelled:
		return nil
	default:
		return fmt.Errorf("agent returned an invalid stop reason %q", reason)
	}
}

func usageFromACP(usage *acpsdk.Usage) harness.Usage {
	if usage == nil {
		return harness.Usage{}
	}
	cacheRead := optionalACPTokenCount(usage.CachedReadTokens)
	cacheCreation := optionalACPTokenCount(usage.CachedWriteTokens)
	reasoning := optionalACPTokenCount(usage.ThoughtTokens)
	return harness.Usage{
		InputTokens:              tokenCount64(usage.InputTokens) + cacheRead + cacheCreation,
		OutputTokens:             tokenCount64(usage.OutputTokens) + reasoning,
		ReasoningTokens:          reasoning,
		CacheReadInputTokens:     cacheRead,
		CacheCreationInputTokens: cacheCreation,
	}
}

func optionalACPTokenCount(value *int64) int64 {
	if value == nil {
		return 0
	}
	return tokenCount64(*value)
}

func tokenCount64(value int64) int64 {
	return int64(max(value, 0))
}

func (a *Agent) cancelPrompt(id acpsdk.SessionId) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = acp.Call(ctx, func() (struct{}, error) {
		return struct{}{}, a.conn.Cancel(ctx, acpsdk.CancelNotification{SessionId: id})
	})
}

func (a *Agent) TurnFeatures(string) code.TurnFeatures {
	return code.TurnFeatures{Steer: a.steer != nil}
}

func (a *Agent) Steer(ctx context.Context, id string, input code.TurnInput) error {
	if a.steer == nil {
		return code.ErrNoActiveTurn
	}
	sess := a.session(id)
	if sess == nil {
		return fmt.Errorf("session %s not found", id)
	}
	sess.mu.Lock()
	t := sess.inflight
	sess.mu.Unlock()
	if t == nil {
		return code.ErrNoActiveTurn
	}
	input.Content = harness.CloneContent(input.Content)
	if err := a.steer(ctx, sess.id, acp.ContentToBlocks(input.Content), input.ID); err != nil {
		return err
	}
	sess.mu.Lock()
	steered := harness.Message{Role: harness.RoleUser, Content: input.Content, InputID: input.ID}
	if sess.inflight == t {
		t.mu.Lock()
		t.emitted = append(t.emitted, steered)
		t.mu.Unlock()
	} else {
		// The backend accepted the steer at the turn boundary, after our stream
		// finalized. Keep the accepted user input visible without retrying it.
		sess.messages = append(sess.messages, steered)
	}
	sess.mu.Unlock()
	return nil
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

func (a *Agent) Close() error {
	// An agent that never started has nothing to close.
	if a == nil {
		return nil
	}
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

		if a.caps.SessionCapabilities.Close != nil && len(sessions) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for _, sess := range sessions {
				if ctx.Err() != nil {
					break
				}
				_, _ = acp.Call(ctx, func() (acpsdk.CloseSessionResponse, error) {
					return a.conn.CloseSession(ctx, acpsdk.CloseSessionRequest{SessionId: sess.id})
				})
			}
		}

		a.abortTransport()

		if a.serverDone != nil {
			select {
			case <-a.serverDone:
			case <-time.After(2 * time.Second):
			}
		}
		if a.serverW != nil {
			_ = a.serverW.Close()
		}
		if a.cleanup != nil {
			_ = a.cleanup()
		}

		if a.cmd == nil || a.cmd.Process == nil {
			return
		}
		select {
		case <-a.processDone:
		case <-time.After(2 * time.Second):
			_ = a.cmd.Process.Kill()
			<-a.processDone
		}
	})
}

func (a *Agent) SessionUpdate(_ context.Context, n acpsdk.SessionNotification) error {
	sess := a.sessionOrDefer(string(n.SessionId), n.Update)
	if sess == nil {
		return nil
	}
	if notice := n.Update.Notice; notice != nil {
		if sess.initialNotices != nil {
			sess.initialNotices()
		}
		a.showNotice(string(n.SessionId), notice)
		return nil
	}
	if a.applySessionStateUpdate(sess, n.Update) {
		a.notifySessionChange(string(n.SessionId))
		return nil
	}
	sess.mu.Lock()
	t := sess.inflight
	sess.mu.Unlock()
	if t == nil {
		return nil
	}
	t.updateMu.Lock()
	defer t.updateMu.Unlock()
	select {
	case <-t.ctx.Done():
		return nil
	case <-t.done:
		return nil
	default:
	}
	msg, ok := a.translateUpdate(sess, t, n.Update)
	if !ok {
		return nil
	}
	t.send(msg)
	return nil
}

func (a *Agent) showNotice(id string, notice *acpsdk.SessionUpdateNotice) {
	text := notice.Title
	if notice.Description != nil && *notice.Description != "" {
		text += "\n" + *notice.Description
	}
	advisory := code.Notice{Severity: string(notice.Severity), Message: text}
	if ui, ok := a.currentUI().(interface{ Notice(string, code.Notice) }); ok {
		ui.Notice(id, advisory)
	} else {
		fmt.Fprintln(os.Stderr, advisory)
	}
}

func isSessionStateUpdate(update acpsdk.SessionUpdate) bool {
	return update.ConfigOptionUpdate != nil || update.CurrentModeUpdate != nil ||
		update.AvailableCommandsUpdate != nil || update.SessionInfoUpdate != nil || update.UsageUpdate != nil
}

// sessionOrDefer looks up and defers under one lock so a concurrent registration cannot strand the update.
func (a *Agent) sessionOrDefer(id string, update acpsdk.SessionUpdate) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sess := a.sessions[id]; sess != nil {
		return sess
	}
	if id == "" || (!isSessionStateUpdate(update) && update.Notice == nil) {
		return nil
	}
	updates, exists := a.pending[id]
	if !exists && len(a.pending) >= 128 {
		return nil
	}
	if len(updates) < 32 {
		if a.pending == nil {
			a.pending = make(map[string][]acpsdk.SessionUpdate)
		}
		a.pending[id] = append(updates, update)
	}
	return nil
}

// applyPendingLocked must run before sess is published so deferred updates cannot overwrite newer direct ones.
func (a *Agent) applyPendingLocked(id string, sess *sessionState) {
	var notices []*acpsdk.SessionUpdateNotice
	for _, update := range a.pending[id] {
		if update.Notice != nil {
			notices = append(notices, update.Notice)
		} else {
			a.applySessionStateUpdate(sess, update)
		}
	}
	if len(notices) != 0 {
		// UI callbacks run outside the agent lock. A live notice racing
		// registration must wait for the earlier notices to be delivered.
		sess.initialNotices = sync.OnceFunc(func() {
			for _, notice := range notices {
				a.showNotice(id, notice)
			}
		})
	}
	delete(a.pending, id)
}

func (a *Agent) applySessionStateUpdate(sess *sessionState, update acpsdk.SessionUpdate) bool {
	if config := update.ConfigOptionUpdate; config != nil {
		a.refreshConfig(sess, config.ConfigOptions)
		return true
	}
	if mode := update.CurrentModeUpdate; mode != nil {
		sess.mu.Lock()
		sess.modeID = string(mode.CurrentModeId)
		sess.mu.Unlock()
		return true
	}
	if commandsUpdate := update.AvailableCommandsUpdate; commandsUpdate != nil {
		commands := make([]code.Command, 0, len(commandsUpdate.AvailableCommands))
		for _, command := range commandsUpdate.AvailableCommands {
			item := code.Command{Name: command.Name, Description: command.Description}
			if command.Input != nil && command.Input.Unstructured != nil {
				item.InputHint = command.Input.Unstructured.Hint
			}
			commands = append(commands, item)
		}
		sess.mu.Lock()
		sess.commands = commands
		sess.mu.Unlock()
		return true
	}
	if info := update.SessionInfoUpdate; info != nil {
		sess.mu.Lock()
		if !info.Title.IsUnset() {
			sess.title, _ = info.Title.Value()
		}
		if info.UpdatedAt.IsNull() {
			sess.updatedAt = time.Time{}
		} else if info.UpdatedAt.Ptr() != nil {
			if parsed, err := time.Parse(time.RFC3339, *info.UpdatedAt.Ptr()); err == nil {
				sess.updatedAt = parsed
			}
		}
		sess.mu.Unlock()
		return true
	}
	if usage := update.UsageUpdate; usage != nil {
		sess.mu.Lock()
		sess.usage.LastInputTokens = int64(usage.Used)
		sess.usage.ContextWindow = int64(usage.Size)
		sess.mu.Unlock()
		return true
	}
	return false
}

func (a *Agent) translateUpdate(sess *sessionState, t *turn, u acpsdk.SessionUpdate) (harness.Message, bool) {
	emit := func(role harness.MessageRole, c harness.Content, contentKey string) harness.Message {
		t.mu.Lock()
		var previous *harness.Content
		if n := len(t.emitted); n > 0 && t.emitted[n-1].Role == role && t.lastContentKey == contentKey {
			content := t.emitted[n-1].Content
			if len(content) > 0 {
				previous = &content[len(content)-1]
			}
		}
		if c.Text != "" && c.TextID == "" {
			if previous != nil && previous.Text != "" {
				c.TextID = previous.TextID
			}
			if c.TextID == "" {
				c.TextID = uuid.NewString()
			}
		}
		if c.Reasoning != nil && c.Reasoning.ID == "" {
			if previous != nil && previous.Reasoning != nil {
				c.Reasoning.ID = previous.Reasoning.ID
			}
			if c.Reasoning.ID == "" {
				c.Reasoning.ID = uuid.NewString()
			}
		}
		if n := len(t.emitted); n > 0 && t.emitted[n-1].Role == role && t.lastContentKey != "v2-message" {
			contents := t.emitted[n-1].Content
			if len(contents) == 0 || t.lastContentKey != contentKey || !mergeACPContent(&contents[len(contents)-1], c) {
				t.emitted[n-1].Content = append(contents, c)
			}
		} else {
			t.emitted = append(t.emitted, harness.Message{Role: role, Content: []harness.Content{c}})
		}
		t.lastContentKey = contentKey
		// The live delta must not share pointers with the accumulated history.
		// Later reasoning chunks mutate that history while the UI reads deltas.
		delta := harness.CloneContent([]harness.Content{c})
		t.mu.Unlock()
		return harness.Message{Role: role, Content: delta}
	}

	switch {
	case u.UserMessageChunk != nil:
		if t.ignoreUserUpdates {
			return harness.Message{}, false
		}
		content, ok := messageBlockContent(u.UserMessageChunk.Content)
		if !ok {
			return harness.Message{}, false
		}
		id := ""
		if u.UserMessageChunk.MessageId != nil {
			id = string(*u.UserMessageChunk.MessageId)
		}
		return emit(harness.RoleUser, content, "user:"+id), true

	case u.AgentMessageChunk != nil:
		content, ok := messageBlockContent(u.AgentMessageChunk.Content)
		if !ok {
			return harness.Message{}, false
		}
		id := ""
		if u.AgentMessageChunk.MessageId != nil {
			id = string(*u.AgentMessageChunk.MessageId)
		}
		if content.Text != "" {
			content.TextID = id
		}
		return emit(harness.RoleAssistant, content, "agent:"+id), true

	case u.AgentThoughtChunk != nil:
		text := blockText(u.AgentThoughtChunk.Content)
		if text == "" {
			return harness.Message{}, false
		}
		id := ""
		if u.AgentThoughtChunk.MessageId != nil {
			id = string(*u.AgentThoughtChunk.MessageId)
		}
		return emit(harness.RoleAssistant, harness.Content{Reasoning: &harness.Reasoning{ID: id, Summary: text}}, "thought:"+id), true

	case u.ToolCall != nil:
		tc := u.ToolCall
		args := rawValueToString(tc.RawInput)
		kind := semanticToolKind(tc.Kind, tc.Content)
		locations := agentToolLocations(a.workspaceRoot(), tc.Locations)

		name := tc.Title
		if name == "" {
			name = string(tc.Kind)
		}
		presentation := harness.NewToolPresentation(name, kind, args, locations)
		sess.toolCallsMu.Lock()
		sess.toolCalls[string(tc.ToolCallId)] = toolCall{
			name: name, kind: kind, args: args, locations: locations,
			content: toolCallContentBlocks(tc.Content), rawOutput: rawValueToString(tc.RawOutput),
		}
		sess.toolCallsMu.Unlock()
		callMsg := emit(harness.RoleAssistant, harness.Content{ToolCall: &harness.ToolCall{
			ID:           string(tc.ToolCallId),
			Name:         name,
			Kind:         kind,
			Args:         args,
			Locations:    locations,
			Presentation: presentation,
		}}, "")

		// A tool_call may arrive already terminal, with no tool_call_update to follow.
		if tc.Status == acpsdk.ToolCallStatusCompleted || tc.Status == acpsdk.ToolCallStatusFailed {
			sess.toolCallsMu.Lock()
			delete(sess.toolCalls, string(tc.ToolCallId))
			sess.toolCallsMu.Unlock()

			body := toolCallContentBlocks(tc.Content)
			if toolContentEmpty(body) && tc.RawOutput != nil {
				body = tool.TextContent(rawValueToString(tc.RawOutput))
			}
			failed := tc.Status == acpsdk.ToolCallStatusFailed
			if toolContentEmpty(body) && failed {
				body = tool.TextContent("tool call failed")
			}
			t.send(callMsg)
			return emit(harness.RoleAssistant, harness.Content{ToolResult: &harness.ToolResult{
				ID:           string(tc.ToolCallId),
				Name:         name,
				Kind:         kind,
				Args:         args,
				Locations:    locations,
				Presentation: presentation,
				Content:      body,
				IsError:      failed,
			}}, ""), true
		}
		return callMsg, true

	case u.ToolCallUpdate != nil:
		tu := u.ToolCallUpdate
		content, hasContent := tu.Content.Value()
		sess.toolCallsMu.Lock()
		prior := sess.toolCalls[string(tu.ToolCallId)]
		if prior.name == "" && tu.Title.Ptr() != nil && *tu.Title.Ptr() != "" {
			prior.name = *tu.Title.Ptr()
		}
		if raw, ok := tu.RawInput.Value(); ok {
			prior.args = rawValueToString(raw)
		}
		if tu.Kind.Ptr() != nil {
			prior.kind = semanticToolKind(*tu.Kind.Ptr(), content)
		} else if prior.kind == "" {
			prior.kind = semanticToolKind("", content)
		}
		if locations, ok := tu.Locations.Value(); ok {
			prior.locations = agentToolLocations(a.workspaceRoot(), locations)
		}
		if hasContent {
			// Updates replace content when present; omitted content retains the
			// latest snapshot, including on a status-only completion.
			prior.content = toolCallContentBlocks(content)
		}
		if raw, ok := tu.RawOutput.Value(); ok {
			prior.rawOutput = rawValueToString(raw)
		}
		sess.toolCalls[string(tu.ToolCallId)] = prior
		sess.toolCallsMu.Unlock()
		if tu.Status.Ptr() == nil {
			a.reportToolProgress(t, tu)
			return harness.Message{}, false
		}
		status := *tu.Status.Ptr()
		if status != acpsdk.ToolCallStatusCompleted && status != acpsdk.ToolCallStatusFailed {
			a.reportToolProgress(t, tu)
			return harness.Message{}, false
		}
		sess.toolCallsMu.Lock()
		prior = sess.toolCalls[string(tu.ToolCallId)]
		delete(sess.toolCalls, string(tu.ToolCallId))
		sess.toolCallsMu.Unlock()
		body := prior.content
		if toolContentEmpty(body) {
			body = tool.TextContent(prior.rawOutput)
		}
		if toolContentEmpty(body) && status == acpsdk.ToolCallStatusFailed {
			body = tool.TextContent("tool call failed")
		}
		return emit(harness.RoleAssistant, harness.Content{ToolResult: &harness.ToolResult{
			ID:        string(tu.ToolCallId),
			Name:      prior.name,
			Kind:      prior.kind,
			Args:      prior.args,
			Locations: prior.locations,
			Presentation: harness.NewToolPresentation(
				prior.name, prior.kind, prior.args, prior.locations,
			),
			Content: body,
			IsError: status == acpsdk.ToolCallStatusFailed,
		}}, ""), true

	}
	return harness.Message{}, false
}

func messageBlockContent(block acpsdk.ContentBlock) (harness.Content, bool) {
	contents := acp.ContentFromBlocks([]acpsdk.ContentBlock{block})
	if len(contents) != 1 {
		return harness.Content{}, false
	}
	return contents[0], true
}

func (a *Agent) reportToolProgress(t *turn, update *acpsdk.SessionToolCallUpdate) {
	if t == nil || t.ctx == nil || update == nil {
		return
	}
	text := ""
	if update.Title.Ptr() != nil {
		text = *update.Title.Ptr()
	}
	if text == "" {
		content, _ := update.Content.Value()
		text = toolCallContentText(content)
	}
	if raw, ok := update.RawOutput.Value(); text == "" && ok {
		text = rawValueToString(raw)
	}
	if text == "" {
		return
	}
	if progress := tool.Progress(tool.WithProgressCall(t.ctx, string(update.ToolCallId))); progress != nil {
		progress(text)
	}
}

// mergeACPContent keeps transport chunks as deltas for the live stream while
// storing them as one logical content block for transcript rendering. Without
// this, a tokenized Codex response becomes one finalized TUI cell per token.
func mergeACPContent(dst *harness.Content, src harness.Content) bool {
	if dst.Text != "" && src.Text != "" {
		dst.Text += src.Text
		return true
	}
	if dst.Reasoning != nil && src.Reasoning != nil && dst.Reasoning.ID == src.Reasoning.ID {
		dst.Reasoning.Summary += src.Reasoning.Summary
		return true
	}
	return false
}

func (a *Agent) SetUI(ui code.UI) {
	a.uiMu.Lock()
	a.ui = ui
	a.uiMu.Unlock()
}

func (a *Agent) currentUI() code.UI {
	a.uiMu.RLock()
	defer a.uiMu.RUnlock()
	return a.ui
}

func (a *Agent) RequestPermission(ctx context.Context, p acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	cancelled := acpsdk.RequestPermissionResponse{
		Outcome: acpsdk.RequestPermissionOutcome{Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{}},
	}
	ctx, release := a.interactionContext(ctx, string(p.SessionId))
	defer release()
	if ctx.Err() != nil {
		return cancelled, nil
	}
	if len(p.Options) == 0 {
		return cancelled, nil
	}
	selected := func(id acpsdk.PermissionOptionId) acpsdk.RequestPermissionResponse {
		if ctx.Err() != nil {
			return cancelled
		}
		return acpsdk.RequestPermissionResponse{
			Outcome: acpsdk.RequestPermissionOutcome{
				Selected: &acpsdk.RequestPermissionOutcomeSelected{OptionId: id},
			},
		}
	}
	if sess := a.session(string(p.SessionId)); sess != nil {
		sess.mu.Lock()
		unattended := sess.modeID == code.UnattendedModeID
		sess.mu.Unlock()
		if unattended {
			if opt := pickPermissionOption(p.Options, true); opt != nil {
				return selected(opt.OptionId), nil
			}
			return cancelled, nil
		}
	}

	ui := a.currentUI()
	if ui == nil {
		if opt := pickPermissionOption(p.Options, false); opt != nil {
			return selected(opt.OptionId), nil
		}
		return cancelled, nil
	}

	// Preferred: a strict single-choice elicitation so the user can pick any
	// of the offered options (answers, always-allow scopes, selects) instead
	// of a yes/no collapse.
	names := make([]string, len(p.Options))
	descriptions := make([]string, len(p.Options))
	hasDescription := false
	for i, o := range p.Options {
		names[i] = o.Name
		descriptions[i] = permissionOptionDescription(acpjson.Object(o.Meta))
		hasDescription = hasDescription || descriptions[i] != ""
	}
	if !hasDescription {
		descriptions = nil
	}
	res, err := acp.Call(ctx, func() (tool.ElicitResult, error) {
		return ui.Elicit(code.WithSessionID(ctx, string(p.SessionId)), tool.ElicitRequest{
			Message: permissionMessage(p),
			Fields: []tool.ElicitField{{
				Name:             "choice",
				Type:             "string",
				Required:         true,
				Enum:             names,
				EnumDescriptions: descriptions,
				Strict:           true,
			}},
		})
	})
	if err == nil {
		switch res.Action {
		case tool.ElicitAccept:
			if choice, ok := res.Content["choice"].(string); ok {
				for i := range p.Options {
					if p.Options[i].Name == choice {
						return selected(p.Options[i].OptionId), nil
					}
				}
			}
			return cancelled, nil
		case tool.ElicitDecline:
			if opt := pickPermissionOption(p.Options, false); opt != nil {
				return selected(opt.OptionId), nil
			}
			return cancelled, nil
		default:
			return cancelled, nil
		}
	}

	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cancelled, nil
	}
	ok, err := acp.Call(ctx, func() (bool, error) {
		return ui.Confirm(code.WithSessionID(ctx, string(p.SessionId)), permissionMessage(p))
	})
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

// Wingman advertises form elicitations only. Completion applies to URL mode.
func (a *Agent) CompleteElicitation(context.Context, acpsdk.CompleteElicitationNotification) error {
	return nil
}

func (a *Agent) CreateElicitation(ctx context.Context, p acpsdk.CreateElicitationRequest) (acpsdk.CreateElicitationResponse, error) {
	cancel := acpsdk.CreateElicitationResponse{Cancel: &acpsdk.CreateElicitationCancel{Action: "cancel"}}
	if p.Form == nil {
		return cancel, nil
	}
	var sid string
	if p.Form.Session != nil {
		sid = string(p.Form.Session.SessionId)
	}
	if sid != "" && a.session(sid) == nil {
		return cancel, nil
	}
	if sid == "" && p.Form.Request == nil {
		sid = a.activeSessionID()
	}
	ctx, release := a.interactionContext(ctx, sid)
	defer release()
	if ctx.Err() != nil {
		return cancel, nil
	}
	if sid != "" {
		ctx = code.WithSessionID(ctx, sid)
	}
	req := tool.ElicitRequest{
		Message: p.Form.Message,
		Fields:  elicitFieldsFromSchema(p.Form.RequestedSchema),
	}
	if sess := a.session(sid); sess != nil {
		sess.mu.Lock()
		unattended := sess.modeID == code.UnattendedModeID
		sess.mu.Unlock()
		if unattended {
			return elicitationResponse(code.UnattendedElicitation(req)), nil
		}
	}

	ui := a.currentUI()
	if ui == nil {
		return acpsdk.CreateElicitationResponse{Decline: &acpsdk.CreateElicitationDecline{Action: "decline"}}, nil
	}
	res, err := acp.Call(ctx, func() (tool.ElicitResult, error) {
		return ui.Elicit(ctx, req)
	})
	if err != nil {
		return cancel, nil
	}
	return elicitationResponse(res), nil
}

func elicitationResponse(res tool.ElicitResult) acpsdk.CreateElicitationResponse {
	switch res.Action {
	case tool.ElicitAccept:
		var content map[string]acpsdk.ElicitationContentValue
		if err := json.Unmarshal(acpjson.Raw(res.Content), &content); err != nil && res.Content != nil {
			return acpsdk.CreateElicitationResponse{Cancel: &acpsdk.CreateElicitationCancel{Action: "cancel"}}
		}
		if content == nil {
			content = map[string]acpsdk.ElicitationContentValue{}
		}
		return acpsdk.CreateElicitationResponse{Accept: &acpsdk.CreateElicitationAccept{Action: "accept", Content: content}}
	case tool.ElicitDecline:
		return acpsdk.CreateElicitationResponse{Decline: &acpsdk.CreateElicitationDecline{Action: "decline"}}
	}
	return acpsdk.CreateElicitationResponse{Cancel: &acpsdk.CreateElicitationCancel{Action: "cancel"}}
}

func (a *Agent) activeSessionID() string {
	a.mu.Lock()
	ids := make([]string, 0, len(a.sessions))
	states := make([]*sessionState, 0, len(a.sessions))
	for sid, sess := range a.sessions {
		ids = append(ids, sid)
		states = append(states, sess)
	}
	a.mu.Unlock()

	active := ""
	for i, sess := range states {
		sess.mu.Lock()
		inflight := sess.inflight != nil
		sess.mu.Unlock()
		if inflight {
			if active != "" {
				return ""
			}
			active = ids[i]
		}
	}
	return active
}

func elicitFieldsFromSchema(schema acpsdk.ElicitationSchema) []tool.ElicitField {
	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}

	fields := make([]tool.ElicitField, 0, len(schema.Properties))
	for _, name := range slices.Sorted(maps.Keys(schema.Properties)) {
		prop := acpjson.Object(schema.Properties[name])
		f := tool.ElicitField{Name: name, Required: required[name]}
		f.Title, _ = prop["title"].(string)
		f.Description, _ = prop["description"].(string)
		f.Default = prop["default"]
		if meta, _ := prop["_meta"].(map[string]any); meta != nil {
			if wingman, _ := meta["wingman"].(map[string]any); wingman != nil {
				f.CustomAnswerFor, _ = wingman["customAnswerFor"].(string)
			}
		}

		typ, _ := prop["type"].(string)
		options := prop["oneOf"]
		if typ == "array" {
			f.Multiple = true
			typ = "string"
			if items, ok := prop["items"].(map[string]any); ok {
				for _, key := range []string{"anyOf", "oneOf", "enum"} {
					if v, ok := items[key]; ok {
						options = v
						break
					}
				}
			}
		}
		if options == nil {
			options = prop["enum"]
		}
		f.Type = typ

		values, descriptions := elicitEnumOptions(options)
		if len(values) > 0 {
			f.Enum = values
			f.EnumDescriptions = descriptions
			f.Strict = true
		}
		fields = append(fields, f)
	}
	return fields
}

func elicitEnumOptions(v any) (values, descriptions []string) {
	items, ok := v.([]any)
	if !ok {
		return nil, nil
	}
	hasDescription := false
	for _, item := range items {
		switch o := item.(type) {
		case string:
			values = append(values, o)
			descriptions = append(descriptions, "")
		case map[string]any:
			value, _ := o["const"].(string)
			if value == "" {
				value, _ = o["title"].(string)
			}
			if value == "" {
				continue
			}
			desc, _ := o["description"].(string)
			values = append(values, value)
			descriptions = append(descriptions, desc)
			if desc != "" {
				hasDescription = true
			}
		}
	}
	if !hasDescription {
		descriptions = nil
	}
	return values, descriptions
}

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

func permissionOptionDescription(meta map[string]any) string {
	permission, _ := meta["permission"].(map[string]any)
	if permission == nil {
		return ""
	}
	var changes []any
	switch value := permission["changes"].(type) {
	case []any:
		changes = value
	case []map[string]any:
		changes = make([]any, len(value))
		for i := range value {
			changes[i] = value[i]
		}
	}
	seen := map[string]bool{}
	var descriptions []string
	for _, raw := range changes {
		change, _ := raw.(map[string]any)
		description, _ := change["description"].(string)
		description = strings.TrimSpace(description)
		if description != "" && !seen[description] {
			seen[description] = true
			descriptions = append(descriptions, description)
		}
	}
	return strings.Join(descriptions, "; ")
}

func permissionMessage(p acpsdk.RequestPermissionRequest) string {
	var parts []string
	if p.ToolCall.Title.Ptr() != nil && *p.ToolCall.Title.Ptr() != "" {
		parts = append(parts, *p.ToolCall.Title.Ptr())
	}
	if raw, ok := p.ToolCall.RawInput.Value(); ok {
		m := acpjson.Object(raw)
		if cmd, ok := m["command"].(string); ok && cmd != "" {
			parts = append(parts, "$ "+cmd)
		}
	}
	content, _ := p.ToolCall.Content.Value()
	if t := toolCallContentText(content); t != "" {
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
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return acpsdk.WriteTextFileResponse{}, err
	}
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
			start = int(min(*p.Line-1, int64(len(lines))))
		}
		end := len(lines)
		if p.Limit != nil && *p.Limit > 0 && *p.Limit < int64(end-start) {
			end = start + int(*p.Limit)
		}
		content = strings.Join(lines[start:end], "\n")
	}
	return acpsdk.ReadTextFileResponse{Content: content}, nil
}

func (a *Agent) resolvePath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute: %s", p)
	}
	return filepath.Clean(p), nil
}

func (a *Agent) CreateTerminal(context.Context, acpsdk.CreateTerminalRequest) (acpsdk.CreateTerminalResponse, error) {
	return acpsdk.CreateTerminalResponse{}, errors.New("terminal not supported")
}

// Terminal is not advertised; a zero-valued success would read as real data.
func (a *Agent) KillTerminal(context.Context, acpsdk.KillTerminalRequest) (acpsdk.KillTerminalResponse, error) {
	return acpsdk.KillTerminalResponse{}, acpsdk.NewMethodNotFound("terminal/kill")
}
func (a *Agent) ReleaseTerminal(context.Context, acpsdk.ReleaseTerminalRequest) (acpsdk.ReleaseTerminalResponse, error) {
	return acpsdk.ReleaseTerminalResponse{}, acpsdk.NewMethodNotFound("terminal/release")
}
func (a *Agent) TerminalOutput(context.Context, acpsdk.TerminalOutputRequest) (acpsdk.TerminalOutputResponse, error) {
	return acpsdk.TerminalOutputResponse{}, acpsdk.NewMethodNotFound("terminal/output")
}
func (a *Agent) WaitForTerminalExit(context.Context, acpsdk.WaitForTerminalExitRequest) (acpsdk.WaitForTerminalExitResponse, error) {
	return acpsdk.WaitForTerminalExitResponse{}, acpsdk.NewMethodNotFound("terminal/wait_for_exit")
}

func (a *Agent) refreshConfig(sess *sessionState, options []acpsdk.SessionConfigOption) {
	if options == nil {
		return
	}
	modelID, effortID := "", ""
	modelOptID, effortOptID := "", ""
	var models []model.Model
	var effortOpts []string
	var remoteModes []code.Mode
	var modeOptID, modeID string
	for _, opt := range options {
		if opt.Select == nil {
			continue
		}
		kind := ""
		if opt.Select.Category != nil {
			kind = string(*opt.Select.Category)
		}
		if kind == "" {
			kind = string(opt.Select.Id)
		}
		values := acp.ConfigValues(opt.Select.Options)
		switch kind {
		case "mode":
			modeOptID, modeID = string(opt.Select.Id), string(opt.Select.CurrentValue)
			for _, v := range values {
				mode := code.Mode{ID: string(v.Value), Name: v.Name}
				if v.Description != nil {
					mode.Description = *v.Description
				}
				remoteModes = append(remoteModes, mode)
			}
		case string(acpsdk.SessionConfigOptionCategoryModel):
			modelOptID = string(opt.Select.Id)
			modelID = string(opt.Select.CurrentValue)
			for _, v := range values {
				description := ""
				if v.Description != nil {
					description = *v.Description
				}
				models = append(models, model.Model{
					ID:          string(v.Value),
					Name:        v.Name,
					Description: description,
				})
			}
		case string(acpsdk.SessionConfigOptionCategoryThoughtLevel), effortConfigID:
			effortOptID = string(opt.Select.Id)
			effortID = string(opt.Select.CurrentValue)
			for _, v := range values {
				effortOpts = append(effortOpts, string(v.Value))
			}
		}
	}
	a.configMu.Lock()
	a.models = models
	a.modelID = modelID
	a.effortID = effortID
	a.effortOpts = effortOpts
	a.configMu.Unlock()

	if sess != nil {
		sess.mu.Lock()
		if _, v2 := a.conn.(*remoteV2); v2 {
			sess.modeOptID, sess.modeID, sess.modes = modeOptID, modeID, remoteModes
		}
		sess.models = append(sess.models[:0], models...)
		sess.modelID = modelID
		sess.modelOptID = modelOptID
		sess.effortID = effortID
		sess.effortOptID = effortOptID
		sess.effortOpts = append(sess.effortOpts[:0], effortOpts...)
		sess.mu.Unlock()
	}
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
		if item.Content != nil && item.Content.Content.Text != nil {
			parts = append(parts, item.Content.Content.Text.Text)
		} else if item.Diff != nil {
			if text := diffBlockText(item.Diff); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func toolContentEmpty(content tool.Content) bool {
	return !slices.ContainsFunc(content, func(block tool.ContentBlock) bool {
		return block.Image != nil || (block.Text != nil && *block.Text != "")
	})
}

func toolCallContentBlocks(items []acpsdk.ToolCallContent) tool.Content {
	var blocks tool.Content
	for _, item := range items {
		switch {
		case item.Content != nil:
			content := item.Content.Content
			if content.Text != nil {
				blocks = append(blocks, tool.TextBlock(content.Text.Text))
			}
			if content.Image != nil {
				if img, ok := tool.ParseImageDataURL(fmt.Sprintf("data:%s;base64,%s", content.Image.MimeType, content.Image.Data)); ok {
					blocks = append(blocks, tool.ContentBlock{Image: img})
				} else {
					blocks = append(blocks, tool.TextBlock("[invalid image attachment]"))
				}
			}
		case item.Diff != nil:
			if text := diffBlockText(item.Diff); text != "" {
				blocks = append(blocks, tool.TextBlock(text))
			}
		}
	}
	return blocks
}

func semanticToolKind(kind acpsdk.ToolKind, items []acpsdk.ToolCallContent) string {
	if kind != "" {
		return string(kind)
	}
	for _, item := range items {
		if item.Diff != nil {
			return string(acpsdk.ToolKindEdit)
		}
	}
	return ""
}

func (a *Agent) workspaceRoot() string {
	if a.workspace == nil {
		return ""
	}
	return a.workspace.RootPath
}

func agentToolLocations(workspaceRoot string, locations []acpsdk.ToolCallLocation) []harness.ToolLocation {
	if len(locations) == 0 {
		return nil
	}
	result := make([]harness.ToolLocation, 0, len(locations))
	for _, location := range locations {
		if location.Path == "" {
			continue
		}
		locationPath := location.Path
		if workspaceRoot != "" && filepath.IsAbs(locationPath) {
			if relative, err := filepath.Rel(workspaceRoot, locationPath); err == nil &&
				relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				locationPath = relative
			}
		}
		mapped := harness.ToolLocation{Path: filepath.ToSlash(locationPath)}
		if location.Line != nil {
			mapped.Line = int(min(*location.Line, int64(math.MaxInt)))
		}
		result = append(result, mapped)
	}
	return result
}

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
		for ln := range strings.SplitSeq(strings.TrimSuffix(df.Text, "\n"), "\n") {
			b.WriteString(prefix)
			b.WriteString(ln)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func rawValueToString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return text
		}
	}
	return string(raw)
}

func (a *Agent) SetSessionUpdateHandler(handler func(string)) {
	a.updateMu.Lock()
	a.updateHandler = handler
	a.updateMu.Unlock()
}
