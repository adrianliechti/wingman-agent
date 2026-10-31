package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
)

// ACP v2 is the experimental draft shipped by ACP SDK 2.2. Its session
// operations reuse the v1 agents; requests and entities are converted here.
type v2Connection struct {
	agent        acp.Agent
	rpc          *acp.Connection
	ctx          context.Context
	mu           sync.Mutex
	sessions     map[string]*v2Session
	capabilities acp.AgentCapabilities
	initialized  bool
	initializing bool
}

type v2Session struct {
	ready, closed         bool
	restoring             bool
	ctx                   context.Context
	cancel                context.CancelFunc
	requests              sync.WaitGroup
	config                []acp.SessionConfigOption
	modes                 *acp.SessionModeState
	commands              any
	lastConfig            any
	setup                 <-chan struct{}
	deferred              []map[string]any
	messageIDs            map[string]string
	lastKind, lastMessage string
	turn                  *v2Turn
	interactions          int
	cancelledThrough      uint64
	closedThrough         uint64
}

type v2Turn struct {
	accept       func(string)
	ctx          context.Context
	cancel       context.CancelFunc
	accepted     chan struct{}
	done         chan struct{}
	once         sync.Once
	messageID    string
	sequence     uint64
	acknowledged <-chan struct{}
	response     acp.PromptResponse
	err          error
}

func newV2Connection(agent acp.Agent, out io.Writer, in io.Reader, ready <-chan struct{}) *v2Connection {
	ctx, cancel := context.WithCancel(context.Background())
	c := &v2Connection{agent: agent, ctx: ctx, sessions: map[string]*v2Session{}}
	wire := newV2Wire(out, in)
	c.rpc = acp.NewConnection(func(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
		<-ready
		method, request := wire.request(method)
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
	var rpc *acp.RequestError
	if errors.As(err, &rpc) {
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

func (c *v2Connection) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if method != "initialize" && !initialized {
		return nil, &acp.RequestError{Code: -32600, Message: "Initialize the connection first"}
	}
	switch method {
	case "initialize":
		c.mu.Lock()
		if c.initialized || c.initializing {
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
			ProtocolVersion *uint16             `json:"protocolVersion"`
			Info            *acp.Implementation `json:"info"`
			Capabilities    map[string]any      `json:"capabilities"`
			Meta            map[string]any      `json:"_meta"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.NewInvalidParams(nil)
		}
		if p.ProtocolVersion == nil {
			return nil, acp.NewInvalidParams(map[string]any{"reason": "protocolVersion is required"})
		}
		if *p.ProtocolVersion < 2 {
			return nil, &acp.RequestError{Code: -32022, Message: "Unsupported protocol version", Data: map[string]any{"requested": *p.ProtocolVersion, "supported": []int{1, 2}}}
		}
		if p.Info == nil || p.Info.Name == "" {
			return nil, acp.NewInvalidParams(map[string]any{"reason": "info.name is required"})
		}
		if auth, ok := p.Capabilities["auth"].(map[string]any); ok {
			auth["terminal"] = auth["terminal"] != nil
		}
		var capabilities acp.ClientCapabilities
		decodeObject(p.Capabilities, &capabilities)
		capabilities.Fs = acp.FileSystemCapabilities{}
		capabilities.Terminal = false
		response, err := c.agent.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: 1, ClientInfo: p.Info, ClientCapabilities: capabilities, Meta: p.Meta})
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
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
		return c.prompt(ctx, p)
	case "session/cancel":
		p, err := decodeRequest[acp.CancelNotification](raw)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		request, _ := ctx.Value(v2RequestKey{}).(v2Request)
		s := c.sessionLocked(string(p.SessionId))
		s.cancelledThrough = max(s.cancelledThrough, request.sequence)
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
		var turn *v2Turn
		c.mu.Lock()
		s := c.sessions[string(p.SessionId)]
		if s != nil {
			request, _ := ctx.Value(v2RequestKey{}).(v2Request)
			s.cancelledThrough = max(s.cancelledThrough, request.sequence)
			s.closedThrough = max(s.closedThrough, request.sequence)
			s.closed = true
			s.cancel()
			turn = s.turn
		}
		c.mu.Unlock()
		if turn != nil {
			turn.cancel()
			<-turn.done
		}
		if s != nil {
			s.requests.Wait()
		}
		return c.agent.CloseSession(ctx, p)
	case "session/list":
		p, err := decodeRequest[acp.ListSessionsRequest](raw)
		if err != nil {
			return nil, err
		}
		response, err := c.agent.ListSessions(ctx, p)
		if err != nil {
			return nil, err
		}
		result := object(response)
		if result["sessions"] == nil {
			result["sessions"] = []any{}
		}
		return result, nil
	case "session/delete":
		p, err := decodeRequest[acp.UnstableDeleteSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		if agent, ok := c.agent.(interface {
			UnstableDeleteSession(context.Context, acp.UnstableDeleteSessionRequest) (acp.UnstableDeleteSessionResponse, error)
		}); ok {
			return agent.UnstableDeleteSession(ctx, p)
		}
	case "session/set_config_option":
		return c.setConfig(ctx, raw)
	case "auth/login":
		p, err := decodeRequest[acp.AuthenticateRequest](raw)
		if err != nil {
			return nil, err
		}
		return c.agent.Authenticate(ctx, p)
	case "auth/logout":
		p, err := decodeRequest[acp.LogoutRequest](raw)
		if err != nil {
			return nil, err
		}
		return c.agent.Logout(ctx, p)
	default:
		if len(method) > 0 && method[0] == '_' {
			if agent, ok := c.agent.(acp.ExtensionMethodHandler); ok {
				return agent.HandleExtensionMethod(ctx, method, raw)
			}
		}
	}
	return nil, acp.NewMethodNotFound(method)
}

func (c *v2Connection) sessionLocked(id string) *v2Session {
	s := c.sessions[id]
	if s == nil {
		ctx, cancel := context.WithCancel(c.ctx)
		s = &v2Session{ctx: ctx, cancel: cancel, messageIDs: map[string]string{}}
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
		if request.sequence <= s.closedThrough {
			c.mu.Unlock()
			return nil, context.Canceled
		}
		if s.restoring {
			c.mu.Unlock()
			return nil, &acp.RequestError{Code: -32600, Message: "Session is already being restored"}
		}
		if s.turn != nil {
			defer c.mu.Unlock()
			if replay {
				return nil, &acp.RequestError{Code: -32600, Message: "Wait for the active turn before replaying history"}
			}
			state := "running"
			if s.interactions > 0 {
				state = "requires_action"
			}
			if err := c.sendLocked(ctx, id, map[string]any{"sessionUpdate": "state_update", "state": state}); err != nil {
				return nil, err
			}
			result := map[string]any{"configOptions": v2Config(s.config, s.modes)}
			if s.commands != nil {
				result["availableCommands"] = s.commands
			}
			return result, nil
		}
		wasReady, wasClosed := s.ready, s.closed
		s.restoring = true
		s.requests.Add(1)
		if s.ctx.Err() != nil {
			s.ctx, s.cancel = context.WithCancel(c.ctx)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		stop := context.AfterFunc(s.ctx, cancel)
		s.closed, s.ready = false, true
		s.messageIDs = map[string]string{}
		s.lastKind, s.lastMessage = "", ""
		c.mu.Unlock()
		defer func() {
			stop()
			cancel()
			c.mu.Lock()
			s.restoring = false
			if err != nil && !s.closed {
				s.ready, s.closed = wasReady, wasClosed
			}
			c.mu.Unlock()
			s.requests.Done()
		}()
	}
	data, _ := json.Marshal(p)
	var response any
	switch method {
	case "session/new":
		request, e := decodeRequest[acp.NewSessionRequest](data)
		if e != nil {
			return nil, e
		}
		response, err = c.agent.NewSession(ctx, request)
	case "session/resume":
		if replay {
			request, e := decodeRequest[acp.LoadSessionRequest](data)
			if e != nil {
				return nil, e
			}
			if agent, ok := c.agent.(interface {
				LoadSession(context.Context, acp.LoadSessionRequest) (acp.LoadSessionResponse, error)
			}); ok {
				response, err = agent.LoadSession(ctx, request)
			} else {
				err = acp.NewMethodNotFound("session/resume")
			}
		} else {
			request, e := decodeRequest[acp.ResumeSessionRequest](data)
			if e != nil {
				return nil, e
			}
			response, err = c.agent.ResumeSession(ctx, request)
		}
	case "session/fork":
		request, e := decodeRequest[acp.UnstableForkSessionRequest](data)
		if e != nil {
			return nil, e
		}
		if agent, ok := c.agent.(interface {
			UnstableForkSession(context.Context, acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error)
		}); ok {
			response, err = agent.UnstableForkSession(ctx, request)
		} else {
			err = acp.NewMethodNotFound(method)
		}
	}
	if err != nil {
		return nil, err
	}
	result := object(response)
	if newID, ok := result["sessionId"].(string); ok {
		id = newID
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sessionLocked(id)
	if s.closed && request.sequence <= s.closedThrough {
		return nil, context.Canceled
	}
	s.ready, s.closed = true, false
	if s.ctx.Err() != nil {
		s.ctx, s.cancel = context.WithCancel(c.ctx)
	}
	s.config = configFromObject(result["configOptions"])
	if modes := result["modes"]; modes != nil {
		decodeObject(modes, &s.modes)
	}
	result["configOptions"] = v2Config(s.config, s.modes)
	s.lastConfig = result["configOptions"]
	delete(result, "modes")
	if s.commands != nil {
		result["availableCommands"] = s.commands
	}
	if method == "session/resume" {
		state := "idle"
		if s.turn != nil {
			state = "running"
			if s.interactions > 0 {
				state = "requires_action"
			}
		}
		if err := c.sendLocked(ctx, id, map[string]any{"sessionUpdate": "state_update", "state": state}); err != nil {
			return nil, err
		}
	}
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
		s.setup = nil
		for _, update := range s.deferred {
			_ = c.sendLocked(c.ctx, id, update)
		}
		s.deferred = nil
	}()
	return result, nil
}

func (c *v2Connection) prompt(ctx context.Context, p acp.PromptRequest) (any, error) {
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
		return nil, acp.NewInvalidParams(map[string]any{"sessionId": id})
	}
	if s.turn != nil || s.restoring {
		c.mu.Unlock()
		return nil, &acp.RequestError{Code: -32600, Message: "Session already has an active prompt"}
	}
	turnCtx, cancel := context.WithCancel(s.ctx)
	t := &v2Turn{ctx: turnCtx, cancel: cancel, accepted: make(chan struct{}), done: make(chan struct{}), sequence: request.sequence, acknowledged: request.sent}
	if p.MessageId != nil {
		t.messageID = *p.MessageId
	}
	if t.messageID == "" {
		t.messageID = uuid.NewString()
	}
	p.MessageId = new(t.messageID)
	stopRequestCancel := context.AfterFunc(ctx, cancel)
	accept := func(messageID string) {
		t.once.Do(func() {
			if turnCtx.Err() != nil {
				return
			}
			if messageID != "" {
				t.messageID = messageID
			}
			stopRequestCancel()
			c.mu.Lock()
			_ = c.sendLocked(turnCtx, id, map[string]any{"sessionUpdate": "user_message", "messageId": t.messageID, "content": p.Prompt})
			_ = c.sendLocked(turnCtx, id, map[string]any{"sessionUpdate": "state_update", "state": "running"})
			c.mu.Unlock()
			close(t.accepted)
		})
	}
	t.accept = accept
	s.turn = t
	s.lastKind, s.lastMessage = "", ""
	c.mu.Unlock()
	go func() {
		defer close(t.done)
		defer cancel()
		t.response, t.err = c.agent.Prompt(context.WithValue(turnCtx, acceptedPromptKey{}, accept), p)
		if t.err == nil && t.response.StopReason != acp.StopReasonCancelled {
			accept("")
		}
		stopRequestCancel()
		select {
		case <-t.accepted:
			select {
			case <-t.acknowledged:
			case <-turnCtx.Done():
				// A close in the same JSON-RPC batch must be able to finish
				// before the batch (including this acknowledgement) is written.
			case <-c.ctx.Done():
			}
		default:
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if s.turn == t {
			s.turn = nil
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
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *v2Connection) setConfig(ctx context.Context, raw json.RawMessage) (any, error) {
	var p map[string]any
	if json.Unmarshal(raw, &p) != nil || p["type"] != "id" {
		return nil, acp.NewInvalidParams(map[string]any{"reason": "only id config values are supported"})
	}
	delete(p, "type")
	id, _ := p["sessionId"].(string)
	c.mu.Lock()
	s := c.sessions[id]
	if s == nil || !s.ready || s.closed {
		c.mu.Unlock()
		return nil, acp.NewInvalidParams(map[string]any{"sessionId": id})
	}
	mode := s != nil && s.modes != nil && p["configId"] == "mode"
	valid := false
	for _, value := range v2Config(s.config, s.modes) {
		option := value.(map[string]any)
		if option["configId"] != p["configId"] {
			continue
		}
		values, _ := option["options"].([]any)
		for _, value := range values {
			entry := value.(map[string]any)
			if entry["value"] == p["value"] {
				valid = true
			}
			if group, ok := entry["options"].([]any); ok {
				for _, item := range group {
					if item.(map[string]any)["value"] == p["value"] {
						valid = true
					}
				}
			}
		}
	}
	c.mu.Unlock()
	if !valid {
		return nil, acp.NewInvalidParams(map[string]any{"reason": "unknown config option or value"})
	}
	var options []acp.SessionConfigOption
	if mode {
		value, ok := p["value"].(string)
		if !ok {
			return nil, acp.NewInvalidParams(nil)
		}
		if _, err := c.agent.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: acp.SessionId(id), ModeId: acp.SessionModeId(value)}); err != nil {
			return nil, err
		}
		c.mu.Lock()
		s.modes.CurrentModeId = acp.SessionModeId(value)
		options = s.config
		c.mu.Unlock()
	} else {
		data, _ := json.Marshal(p)
		request, err := decodeRequest[acp.SetSessionConfigOptionRequest](data)
		if err != nil {
			return nil, err
		}
		response, err := c.agent.SetSessionConfigOption(ctx, request)
		if err != nil {
			return nil, err
		}
		options = response.ConfigOptions
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s = c.sessionLocked(id)
	s.config = options
	converted := v2Config(options, s.modes)
	if err := c.sendLocked(ctx, id, map[string]any{"sessionUpdate": "config_option_update", "configOptions": converted}); err != nil {
		return nil, err
	}
	return map[string]any{"configOptions": converted}, nil
}
