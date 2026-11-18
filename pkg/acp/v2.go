package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
	"github.com/rogpeppe/go-internal/lockedfile"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

// ACP v2 is the experimental draft shipped by ACP SDK 2.8. Its session
// operations reuse the v1 agents; requests and entities are converted here.
type v2Connection struct {
	agent        acp.Agent
	rpc          *acp.Connection
	ctx          context.Context
	mu           sync.Mutex
	sessions     map[string]*v2Session
	capabilities acp.AgentCapabilities
	agentName    string
	initialized  bool
	initializing bool
	used         bool
}

type v2Session struct {
	ready, closed                         bool
	restoring, replaying, inactiveRestore bool
	closing                               chan struct{}
	closeErr                              error
	workspace                             map[string]any
	info                                  map[string]any
	replayMessage                         int
	replaySequence                        uint64
	afterReplay                           []json.RawMessage
	receipts                              map[string]string
	ctx                                   context.Context
	cancel                                context.CancelFunc
	requests                              sync.WaitGroup
	config                                []acp.SessionConfigOption
	modes                                 *acp.SessionModeState
	commands                              []any
	lastConfig                            any
	setup                                 <-chan struct{}
	deferred                              []map[string]any
	reportedEntities                      map[string]bool
	toolStatuses                          map[string]string
	lastKind, lastMessage                 string
	turn                                  *v2Turn
	pending                               []*v2Turn
	keyed                                 map[string]*v2Turn
	interactions                          int
	cancelledThrough                      uint64
	closedThrough                         uint64
}

type v2Turn struct {
	start        chan struct{}
	accept       func(string)
	ctx          context.Context
	cancel       context.CancelFunc
	accepted     chan struct{}
	done         chan struct{}
	mu           sync.Mutex
	sealed       bool
	messageID    string
	sequence     uint64
	acknowledged <-chan struct{}
	response     acp.PromptResponse
	err          error
}

func newV2Connection(agent acp.Agent, out io.Writer, in io.Reader) *v2Connection {
	ctx, cancel := context.WithCancel(context.Background())
	c := &v2Connection{agent: agent, ctx: ctx, sessions: map[string]*v2Session{}}
	wire := newV2Wire(out, in)
	c.rpc = acp.NewConnection(func(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
		method, request := wire.request(method)
		if request.registered != nil {
			if request.previousPrompt != nil {
				select {
				case <-request.previousPrompt:
				case <-c.ctx.Done():
					return nil, protocolError(c.ctx.Err())
				}
			}
			defer request.registered()
		}
		result, err := c.handle(context.WithValue(ctx, v2RequestKey{}, request), method, raw)
		return result, protocolError(err)
	}, wire, wire)
	go func() { <-c.rpc.Done(); cancel() }()
	return c
}

func (c *v2Connection) Done() <-chan struct{} { return c.rpc.Done() }

func protocolError(err error) *acp.RequestError {
	if err == nil {
		return nil
	}
	if rpc, ok := errors.AsType[*acp.RequestError](err); ok {
		return rpc
	}
	if errors.Is(err, context.Canceled) {
		return &acp.RequestError{Code: -32800, Message: "Request cancelled"}
	}
	return &acp.RequestError{Code: -32603, Message: err.Error()}
}

func decodeRequest[T any](raw json.RawMessage) (T, error) {
	var p T
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	if v, ok := any(&p).(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return p, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
	}
	return p, nil
}

func callRequest[P, R any](ctx context.Context, raw json.RawMessage, call func(context.Context, P) (R, error)) (R, error) {
	request, err := decodeRequest[P](raw)
	if err != nil {
		var zero R
		return zero, err
	}
	return call(ctx, request)
}

// IsV2Request reports whether a lifecycle request came through the v2
// dispatcher. V1 can make history optional, while v2 requires active replay.
func IsV2Request(ctx context.Context) bool {
	_, v2 := ctx.Value(v2RequestKey{}).(v2Request)
	return v2
}

func (c *v2Connection) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	c.mu.Lock()
	initialized := c.initialized && !c.initializing
	if initialized && method != "initialize" {
		c.used = true
	}
	c.mu.Unlock()
	if method != "initialize" && !initialized {
		return nil, &acp.RequestError{Code: -32600, Message: "Initialize the connection first"}
	}
	switch method {
	case "initialize":
		c.mu.Lock()
		if c.used || c.initializing {
			c.mu.Unlock()
			return nil, &acp.RequestError{Code: -32600, Message: "Connection is already initialized"}
		}
		c.initializing = true
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			c.initializing = false
			c.mu.Unlock()
		}()
		var p struct {
			Info         *acp.Implementation `json:"info"`
			Capabilities map[string]any      `json:"capabilities"`
			Meta         acp.Meta            `json:"_meta"`
		}
		var version struct {
			ProtocolVersion *uint16 `json:"protocolVersion"`
		}
		if err := json.Unmarshal(raw, &version); err != nil || version.ProtocolVersion == nil {
			return nil, acp.NewInvalidParams(nil)
		}
		if err := json.Unmarshal(raw, &p); err != nil && *version.ProtocolVersion <= 2 {
			return nil, acp.NewInvalidParams(nil)
		}
		if *version.ProtocolVersion < 2 {
			return nil, acp.NewRequestError(-32022, "Unsupported protocol version", map[string]any{"requested": *version.ProtocolVersion, "supported": []int{1, 2}})
		}
		if *version.ProtocolVersion == 2 && (p.Info == nil || p.Info.Name == "" || p.Info.Version == "") {
			return nil, acp.NewInvalidParams(map[string]any{"reason": "info.name and info.version are required"})
		}
		if auth, ok := p.Capabilities["auth"].(map[string]any); ok {
			auth["terminal"] = auth["terminal"] != nil
		}
		var capabilities acp.ClientCapabilities
		decodeObject(p.Capabilities, &capabilities)
		// Notices are part of the v2 baseline. The v1 backend uses this
		// capability to choose between a live notice and a chat message.
		if capabilities.Session == nil {
			capabilities.Session = &acp.ClientSessionCapabilities{}
		}
		capabilities.Session.Notices = &acp.NoticeCapabilities{}
		capabilities.Fs = acp.FileSystemCapabilities{}
		capabilities.Terminal = false
		response, err := c.agent.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: 1, ClientInfo: p.Info, ClientCapabilities: capabilities, Meta: p.Meta})
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if response.AgentInfo != nil {
			c.agentName = response.AgentInfo.Name
		}
		c.capabilities = response.AgentCapabilities
		c.initialized = true
		c.mu.Unlock()
		return v2Initialize(response), nil
	case "session/new", "session/resume", "session/fork":
		return c.setup(ctx, method, raw)
	case "session/prompt":
		p, err := decodeRequest[acp.PromptRequest](raw)
		if err != nil {
			return nil, err
		}
		if len(p.Prompt) == 0 {
			return nil, acp.NewInvalidParams(map[string]any{"reason": "prompt must not be empty"})
		}
		for _, block := range p.Prompt {
			if block.Audio != nil || block.Text == nil && block.Image == nil && block.Resource == nil && block.ResourceLink == nil {
				return nil, acp.NewInvalidParams(map[string]any{"reason": "unsupported prompt content"})
			}
			if block.Image != nil && !c.capabilities.PromptCapabilities.Image || block.Resource != nil && !c.capabilities.PromptCapabilities.EmbeddedContext {
				return nil, acp.NewInvalidParams(map[string]any{"reason": "prompt content was not advertised"})
			}
		}
		var params struct {
			Key *string `json:"idempotencyKey"`
		}
		if err := json.Unmarshal(raw, &params); err != nil || (params.Key != nil && (*params.Key == "" || utf8.RuneCountInString(*params.Key) > 255)) {
			return nil, acp.NewInvalidParams("idempotencyKey must contain 1 to 255 characters")
		}
		var key string
		if params.Key != nil {
			key = *params.Key
		}
		return c.prompt(ctx, p, key)
	case "session/cancel":
		p, err := decodeRequest[acp.CancelNotification](raw)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		request, _ := ctx.Value(v2RequestKey{}).(v2Request)
		s := c.sessions[string(p.SessionId)]
		if s == nil || !s.ready || s.closed {
			c.mu.Unlock()
			return nil, nil
		}
		s.cancelledThrough = max(s.cancelledThrough, request.sequence)
		for _, pending := range s.pending {
			if pending.sequence <= s.cancelledThrough {
				pending.cancel()
			}
		}
		if s.turn != nil && s.turn.sequence <= s.cancelledThrough {
			s.turn.cancel()
		}
		c.mu.Unlock()
		return nil, nil
	case "session/close":
		p, err := decodeRequest[acp.CloseSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return acp.CloseSessionResponse{}, c.closeSession(ctx, p.SessionId, nil)
	case "session/list":
		return callRequest(ctx, raw, c.agent.ListSessions)
	case "session/delete":
		p, err := decodeRequest[acp.DeleteSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		if c.capabilities.SessionCapabilities.Delete == nil {
			return nil, acp.NewMethodNotFound(method)
		}
		err = c.closeSession(ctx, p.SessionId, func() error { _, err := c.agent.DeleteSession(ctx, p); return err })
		return acp.DeleteSessionResponse{}, err
	case "session/set_config_option":
		return c.setConfig(ctx, raw)
	case "auth/login":
		return callRequest(ctx, raw, c.agent.Authenticate)
	case "auth/logout":
		return callRequest(ctx, raw, c.agent.Logout)
	default:
		if len(method) > 0 && method[0] == '_' {
			if agent, ok := c.agent.(acp.ExtensionMethodHandler); ok {
				return agent.HandleExtensionMethod(ctx, method, raw)
			}
		}
	}
	return nil, acp.NewMethodNotFound(method)
}

// Mark the session closed under the registration lock before waiting for
// prompts and interactions. The backend is released after the last idle update.
func (c *v2Connection) closeSession(ctx context.Context, id acp.SessionId, remove func() error) error {
	request, _ := ctx.Value(v2RequestKey{}).(v2Request)
	c.mu.Lock()
	s := c.sessionLocked(string(id))
	for s.closing != nil {
		closing := s.closing
		c.mu.Unlock()
		select {
		case <-closing:
		case <-ctx.Done():
			return ctx.Err()
		}
		c.mu.Lock()
	}
	if s.closed && remove == nil {
		err := s.closeErr
		c.mu.Unlock()
		return err
	}
	active := s.ready || s.restoring
	s.closing = make(chan struct{})
	s.closed = true
	s.closedThrough = max(s.closedThrough, request.sequence)
	s.cancelledThrough = max(s.cancelledThrough, request.sequence)
	s.cancel()
	c.mu.Unlock()
	s.requests.Wait()
	var err error
	if remove != nil {
		err = remove()
	} else if active {
		_, err = c.agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: id})
	}
	c.mu.Lock()
	s.closeErr = err
	close(s.closing)
	s.closing = nil
	c.mu.Unlock()
	return err
}

func (c *v2Connection) sendSnapshot(ctx context.Context, id string, s *v2Session) error {
	if err := c.sendLocked(ctx, id, s.info); err != nil {
		return err
	}
	state := "idle"
	if s.turn != nil {
		state = "running"
		if s.interactions > 0 {
			state = "requires_action"
		}
	}
	return c.sendLocked(ctx, id, map[string]any{"sessionUpdate": "state_update", "state": state})
}

func (c *v2Connection) sessionLocked(id string) *v2Session {
	s := c.sessions[id]
	if s == nil {
		ctx, cancel := context.WithCancel(c.ctx)
		s = &v2Session{
			ctx: ctx, cancel: cancel,
			reportedEntities: map[string]bool{},
			toolStatuses:     map[string]string{},
			keyed:            map[string]*v2Turn{},
			receipts:         map[string]string{},
			info:             map[string]any{"sessionUpdate": "session_info_update"},
		}
		c.sessions[id] = s
	}
	return s
}

func (c *v2Connection) setup(ctx context.Context, method string, raw json.RawMessage) (resultValue any, err error) {
	request, _ := ctx.Value(v2RequestKey{}).(v2Request)
	p, err := v1Setup(raw, c.capabilities.McpCapabilities)
	if err != nil {
		return nil, err
	}
	id, _ := p["sessionId"].(string)
	workspace := map[string]any{"cwd": p["cwd"], "additionalDirectories": p["additionalDirectories"], "mcpServers": p["mcpServers"]}
	if method == "session/fork" {
		c.mu.Lock()
		parent := c.sessions[id]
		active := parent != nil && parent.ready && !parent.closed
		c.mu.Unlock()
		if !active {
			return nil, SessionNotFound(acp.SessionId(id))
		}
		if c.capabilities.SessionCapabilities.Fork == nil {
			return nil, acp.NewMethodNotFound(method)
		}
		if p["cursorId"] != nil {
			// No backend emits cursor_update. A whole-session fork would
			// silently include history beyond the client's requested point.
			return nil, acp.NewInvalidParams(map[string]any{"cursorId": p["cursorId"], "reason": "unknown fork cursor"})
		}
	}
	replay := false
	if value := p["replayFrom"]; value != nil {
		cursor, ok := value.(map[string]any)
		if !ok || cursor["type"] != "start" {
			return nil, acp.NewInvalidParams(map[string]any{"reason": "only replayFrom start is supported"})
		}
		replay = true
	}
	delete(p, "replayFrom")
	if method == "session/resume" {
		c.mu.Lock()
		s := c.sessionLocked(id)
		if previous := s.setup; previous != nil {
			c.mu.Unlock()
			select {
			case <-previous:
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-c.ctx.Done():
				return nil, c.ctx.Err()
			}
			c.mu.Lock()
			if s.setup == previous {
				c.flushSetupLocked(s, id)
			}
		}
		if s.closing != nil || request.sequence <= s.closedThrough {
			c.mu.Unlock()
			return nil, context.Canceled
		}
		if s.restoring {
			c.mu.Unlock()
			return nil, &acp.RequestError{Code: -32600, Message: "Session is already being restored"}
		}
		if s.ready && !s.closed && !replay && reflect.DeepEqual(s.workspace, workspace) && (s.turn != nil || c.capabilities.SessionCapabilities.Resume == nil) {
			defer c.mu.Unlock()
			if err := c.sendSnapshot(ctx, id, s); err != nil {
				return nil, err
			}
			return s.setupResult(), nil
		}
		wasReady, wasClosed, oldWorkspace := s.ready, s.closed, s.workspace
		s.restoring = true
		s.inactiveRestore = !wasReady || wasClosed
		s.requests.Add(1)
		if s.ctx.Err() != nil {
			s.ctx, s.cancel = context.WithCancel(c.ctx)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		stop := context.AfterFunc(s.ctx, cancel)
		s.closed = false
		turn := s.turn
		c.mu.Unlock()
		defer func() {
			stop()
			cancel()
			c.mu.Lock()
			s.restoring, s.replaying, s.inactiveRestore = false, false, false
			s.afterReplay = nil
			s.startNext()
			if err != nil && !s.closed {
				s.ready, s.closed, s.workspace = wasReady, wasClosed, oldWorkspace
			}
			c.mu.Unlock()
			s.requests.Done()
		}()
		// Backend reloads may replace the session object. Finish its running
		// turn first and keep held inputs queued on the same session.
		if turn != nil {
			select {
			case <-turn.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		c.mu.Lock()
		if s.closed {
			c.mu.Unlock()
			return nil, context.Canceled
		}
		s.ready, s.replaying = true, replay
		s.replaySequence = request.sequence
		s.workspace = workspace
		if replay {
			s.reportedEntities = map[string]bool{}
		}
		s.lastKind, s.lastMessage = "", ""
		s.replayMessage = 0
		c.mu.Unlock()
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	response, err := c.openSession(ctx, method, data, replay)
	if err != nil {
		return nil, err
	}
	if method != "session/resume" {
		id = string(response.SessionId)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sessionLocked(id)
	if s.closed && request.sequence <= s.closedThrough {
		return nil, context.Canceled
	}
	s.ready, s.closed = true, false
	s.workspace = workspace
	if s.ctx.Err() != nil {
		s.ctx, s.cancel = context.WithCancel(c.ctx)
	}
	s.config = response.ConfigOptions
	if response.Modes != nil {
		modes := *response.Modes
		s.modes = &modes
	}
	if method == "session/resume" {
		if replay {
			if err := c.sendLocked(ctx, id, map[string]any{"sessionUpdate": "replay_complete"}); err != nil {
				return nil, err
			}
		}
		s.replaying = false
		for _, raw := range s.afterReplay {
			var update acp.SessionUpdate
			if err := json.Unmarshal(raw, &update); err != nil {
				return nil, err
			}
			if err := c.updateLocked(ctx, id, s, update); err != nil {
				return nil, err
			}
		}
		s.afterReplay = nil
		if err := c.sendSnapshot(ctx, id, s); err != nil {
			return nil, err
		}
	}
	result := s.setupResult()
	if method != "session/resume" {
		result["sessionId"] = id
	}
	if len(response.Meta) > 0 {
		result["_meta"] = response.Meta
	}
	s.lastConfig = result["configOptions"]
	s.setup = request.sent
	go func() {
		select {
		case <-request.sent:
		case <-c.ctx.Done():
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if s.setup != request.sent {
			return
		}
		c.flushSetupLocked(s, id)
	}()
	return result, nil
}

func (s *v2Session) setupResult() map[string]any {
	result := map[string]any{"configOptions": v2Config(s.config, s.modes)}
	if s.commands != nil {
		result["availableCommands"] = s.commands
	}
	return result
}

func (c *v2Connection) openSession(ctx context.Context, method string, raw json.RawMessage, replay bool) (acp.NewSessionResponse, error) {
	switch method {
	case "session/new":
		return callRequest(ctx, raw, c.agent.NewSession)
	case "session/fork":
		if agent, ok := c.agent.(acp.AgentUnstableForkSession); ok {
			response, err := callRequest(ctx, raw, agent.UnstableForkSession)
			return acp.NewSessionResponse(response), err
		}
	case "session/resume":
		var response acp.ResumeSessionResponse
		var err error
		if replay {
			loader, ok := c.agent.(acp.AgentLoader)
			if !ok {
				return acp.NewSessionResponse{}, acp.NewMethodNotFound(method)
			}
			loaded, loadErr := callRequest(ctx, raw, loader.LoadSession)
			response, err = acp.ResumeSessionResponse(loaded), loadErr
		} else {
			response, err = callRequest(ctx, raw, c.agent.ResumeSession)
		}
		return acp.NewSessionResponse{Meta: response.Meta, ConfigOptions: response.ConfigOptions, Modes: response.Modes}, err
	}
	return acp.NewSessionResponse{}, acp.NewMethodNotFound(method)
}

func (c *v2Connection) flushSetupLocked(s *v2Session, id string) {
	s.setup = nil
	for _, update := range s.deferred {
		_ = c.sendLocked(c.ctx, id, update)
	}
	s.deferred = nil
}
func (c *v2Connection) prompt(ctx context.Context, p acp.PromptRequest, key string) (any, error) {
	id := string(p.SessionId)
	request, _ := ctx.Value(v2RequestKey{}).(v2Request)
	c.mu.Lock()
	s := c.sessions[id]
	if s != nil && request.sequence <= s.cancelledThrough {
		c.mu.Unlock()
		return nil, context.Canceled
	}
	if s == nil || !s.ready || s.closed {
		c.mu.Unlock()
		return nil, SessionNotFound(acp.SessionId(id))
	}
	if s.inactiveRestore {
		c.mu.Unlock()
		return nil, SessionNotFound(p.SessionId)
	}
	if messageID := s.receipts[key]; key != "" && messageID != "" {
		c.mu.Unlock()
		return map[string]any{"messageId": messageID}, nil
	}
	if prior := s.keyed[key]; key != "" && prior != nil {
		c.mu.Unlock()
		request.registered()
		return prior.wait(ctx)
	}
	turnCtx, cancel := context.WithCancel(s.ctx)
	t := &v2Turn{start: make(chan struct{}), ctx: turnCtx, cancel: cancel, accepted: make(chan struct{}), done: make(chan struct{}), sequence: request.sequence, acknowledged: request.sent}
	t.messageID = uuid.NewString()
	turnCtx = harness.WithInputID(turnCtx, t.messageID)
	var receipt *lockedfile.File
	accept := func(messageID string) {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.sealed {
			return
		}
		t.sealed = true
		if messageID != "" {
			t.messageID = messageID
		}
		if err := finishPromptReceipt(receipt, t.messageID); err != nil {
			slog.Error("save ACP prompt receipt", "err", err)
		}
		receipt = nil
		c.mu.Lock()
		if key != "" {
			s.receipts[key] = t.messageID
		}
		_ = c.sendLocked(c.ctx, id, map[string]any{"sessionUpdate": "user_message", "messageId": t.messageID, "content": p.Prompt})
		_ = c.sendLocked(c.ctx, id, map[string]any{"sessionUpdate": "state_update", "state": "running"})
		c.mu.Unlock()
		close(t.accepted)
	}
	if key != "" {
		s.keyed[key] = t
	}
	s.pending = append(s.pending, t)
	s.startNext()
	s.requests.Add(1)
	request.registered()
	c.mu.Unlock()
	go func() {
		defer s.requests.Done()
		defer close(t.done)
		defer cancel()
		var path, recorded string
		if key != "" {
			path, t.err = promptReceiptPath(c.agentName, id, key)
			if t.err == nil {
				recorded = recordedPrompt(path)
			}
		}
		if recorded == "" && t.err == nil {
			select {
			case <-t.start:
			case <-turnCtx.Done():
			}
			if path != "" && turnCtx.Err() == nil {
				receipt, recorded, t.err = openPromptReceipt(path)
			}
		}
		if recorded != "" {
			c.mu.Lock()
			s.receipts[key] = recorded
			t.messageID = recorded
			close(t.accepted)
			c.mu.Unlock()
		} else if turnCtx.Err() != nil {
			t.err = turnCtx.Err()
		} else if t.err == nil {
			c.mu.Lock()
			t.accept = accept
			c.mu.Unlock()
			t.response, t.err = c.agent.Prompt(context.WithValue(turnCtx, acceptedPromptKey{}, accept), p)
		}
		if recorded == "" && turnCtx.Err() == nil && t.err == nil && t.response.StopReason != acp.StopReasonCancelled {
			accept("")
		}
		t.mu.Lock()
		t.sealed = true
		if err := finishPromptReceipt(receipt, ""); err != nil {
			slog.Error("release ACP prompt receipt", "err", err)
		}
		t.mu.Unlock()
		select {
		case <-t.accepted:
			select {
			case <-t.acknowledged:
			case <-turnCtx.Done():
			case <-c.ctx.Done():
			}
		default:
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if s.turn == t {
			s.turn = nil
		}
		s.pending = slices.DeleteFunc(s.pending, func(p *v2Turn) bool { return p == t })
		delete(s.keyed, key)
		defer s.startNext()
		if recorded != "" {
			return
		}
		select {
		case <-t.accepted:
			update := map[string]any{"sessionUpdate": "state_update", "state": "idle", "stopReason": t.response.StopReason}
			if errors.Is(t.err, context.Canceled) || turnCtx.Err() != nil {
				update["stopReason"] = "cancelled"
			} else if t.err != nil {
				update["stopReason"] = "error"
				update["error"] = protocolError(t.err)
			}
			if t.response.Usage != nil {
				update["usage"] = t.response.Usage
			}
			_ = c.sendLocked(c.ctx, id, update)
		default:
		}
	}()
	return t.wait(ctx)
}

func (s *v2Session) startNext() {
	if s.turn == nil && len(s.pending) > 0 && !s.closed && !s.restoring {
		s.turn = s.pending[0]
		s.pending = s.pending[1:]
		s.lastKind, s.lastMessage = "", ""
		close(s.turn.start)
	}
}

func (t *v2Turn) wait(ctx context.Context) (any, error) {
	// Once accepted, cancelling an RPC must not cancel foreground work.
	select {
	case <-ctx.Done():
		t.mu.Lock()
		if !t.sealed {
			t.cancel()
		}
		t.mu.Unlock()
	case <-t.accepted:
	case <-t.done:
	}
	select {
	case <-t.accepted:
		return map[string]any{"messageId": t.messageID}, nil
	case <-t.done:
		select {
		case <-t.accepted:
			return map[string]any{"messageId": t.messageID}, nil
		default:
		}
		if t.err != nil {
			return nil, t.err
		}
		if t.response.StopReason == acp.StopReasonCancelled {
			return nil, context.Canceled
		}
		return nil, fmt.Errorf("backend ended before accepting the prompt")
	}
}

func (s *v2Session) acceptsConfig(option *acp.SetSessionConfigOptionValueId) bool {
	if s.modes != nil && option.ConfigId == "mode" {
		return slices.ContainsFunc(s.modes.AvailableModes, func(mode acp.SessionMode) bool {
			return mode.Id == acp.SessionModeId(option.Value)
		})
	}
	for _, config := range s.config {
		if config.Select != nil && config.Select.Id == option.ConfigId {
			for _, choice := range ConfigValues(config.Select.Options) {
				if choice.Value == option.Value {
					return true
				}
			}
		}
	}
	return false
}

func (c *v2Connection) setConfig(ctx context.Context, raw json.RawMessage) (any, error) {
	request, err := decodeRequest[acp.SetSessionConfigOptionRequest](raw)
	if err != nil {
		return nil, err
	}
	var kind struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &kind) != nil || kind.Type != "id" || request.ValueId == nil {
		return nil, acp.NewInvalidParams(map[string]any{"reason": "only id config values are supported"})
	}
	option := request.ValueId
	id := string(option.SessionId)
	c.mu.Lock()
	s := c.sessions[id]
	if s == nil || !s.ready || s.closed {
		c.mu.Unlock()
		return nil, SessionNotFound(option.SessionId)
	}
	mode := s.modes != nil && option.ConfigId == "mode"
	valid := s.acceptsConfig(option)
	c.mu.Unlock()
	if !valid {
		return nil, acp.NewInvalidParams(map[string]any{"reason": "unknown config option or value"})
	}
	var response acp.SetSessionConfigOptionResponse
	if mode {
		_, err = c.agent.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: option.SessionId, ModeId: acp.SessionModeId(option.Value)})
	} else {
		response, err = c.agent.SetSessionConfigOption(ctx, request)
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if mode {
		s.modes.CurrentModeId = acp.SessionModeId(option.Value)
	} else {
		s.config = response.ConfigOptions
	}
	converted := v2Config(s.config, s.modes)
	if err := c.sendLocked(ctx, id, map[string]any{"sessionUpdate": "config_option_update", "configOptions": converted}); err != nil {
		return nil, err
	}
	return map[string]any{"configOptions": converted}, nil
}
