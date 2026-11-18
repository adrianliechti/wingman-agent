package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/coder/acp-go-sdk"
)

// ClientConnection is the client surface used by our agents. Both ACP versions
// share the same backend; only the connection translates their wire contracts.
type ClientConnection interface {
	Done() <-chan struct{}
	SessionUpdate(context.Context, acp.SessionNotification) error
	RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
	CreateElicitation(context.Context, acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error)
}

type connectedAgent interface {
	acp.Agent
	SetClientConnection(ClientConnection)
}

// AgentConnection negotiates one version per connection. The v1 SDK remains
// responsible for the entire v1 surface and JSON-RPC framing on both versions.
type AgentConnection struct {
	ClientConnection
	ready     chan struct{}
	done      chan struct{}
	mu        sync.Mutex
	logger    *slog.Logger
	setLogger func(*slog.Logger)
	v1Output  *sessionResponseWriter
}

func NewAgentConnection(agent connectedAgent, out io.Writer, in io.Reader) *AgentConnection {
	c := &AgentConnection{ready: make(chan struct{}), done: make(chan struct{})}
	agent.SetClientConnection(c)
	go func() {
		defer close(c.done)
		reader := bufio.NewReader(in)
		first, version, err := firstInitialize(reader, out)
		if err != nil {
			close(c.ready)
			return
		}
		// The SDK starts its reader inside the constructor. Publish the
		// connection before it can dispatch any already-buffered requests.
		input := io.MultiReader(connectionReady(c.ready), bytes.NewReader(first), reader)
		c.mu.Lock()
		if version == 2 {
			v2 := newV2Connection(agent, out, input)
			c.ClientConnection = v2
			c.setLogger = v2.rpc.SetLogger
		} else {
			c.v1Output = &sessionResponseWriter{out: out}
			v1 := acp.NewAgentSideConnection(agent, c.v1Output, input)
			c.ClientConnection = v1
			c.setLogger = v1.SetLogger
		}
		if c.logger != nil {
			c.setLogger(c.logger)
		}
		c.mu.Unlock()
		close(c.ready)
		<-c.ClientConnection.Done()
	}()
	return c
}

type connectionReady <-chan struct{}

func (ready connectionReady) Read([]byte) (int, error) {
	<-ready
	return 0, io.EOF
}

// AfterV1SessionResponse schedules an announcement for a newly allocated
// session. V2 carries the initial command list in the setup response itself.
func (c *AgentConnection) AfterV1SessionResponse(id acp.SessionId, announce func()) bool {
	<-c.ready
	w := c.v1Output
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == nil {
		w.pending = make(map[acp.SessionId]func())
	}
	w.pending[id] = announce
	return true
}

type sessionResponseWriter struct {
	out     io.Writer
	mu      sync.Mutex
	pending map[acp.SessionId]func()
}

func (w *sessionResponseWriter) Write(frame []byte) (int, error) {
	n, err := w.out.Write(frame)
	if err != nil || n != len(frame) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return n, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) != 0 {
		var response struct {
			ID     json.RawMessage
			Method string
			Result struct{ SessionId acp.SessionId }
		}
		if json.Unmarshal(frame, &response) == nil && response.ID != nil && response.Method == "" {
			if announce := w.pending[response.Result.SessionId]; announce != nil {
				delete(w.pending, response.Result.SessionId)
				// The SDK holds its write lock until this call returns.
				go announce()
			}
		}
	}
	return n, nil
}

// Until a valid version arrives there is no version-specific dispatcher.
func firstInitialize(reader *bufio.Reader, out io.Writer) ([]byte, uint16, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > 10<<20 {
			return nil, 0, fmt.Errorf("ACP frame exceeds 10 MiB")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		var frame struct {
			Method  string
			ID      json.RawMessage
			Params  json.RawMessage
			JSONRPC string
		}
		var failure *acp.RequestError
		if len(bytes.TrimSpace(line)) != 0 {
			switch {
			case !json.Valid(line):
				failure = acp.NewParseError(nil)
			case json.Unmarshal(line, &frame) != nil || frame.JSONRPC != "2.0" || !validRequestID(frame.ID):
				frame.ID = nil
				failure = acp.NewInvalidRequest(nil)
			case frame.ID == nil: // Ignore notifications until initialization.
			case frame.Method != "initialize":
				failure = acp.NewInvalidRequest(nil)
			default:
				var params struct {
					ProtocolVersion *uint16 `json:"protocolVersion"`
				}
				if json.Unmarshal(frame.Params, &params) != nil || params.ProtocolVersion == nil {
					failure = acp.NewInvalidParams(nil)
				} else if *params.ProtocolVersion == 0 {
					failure = acp.NewRequestError(-32022, "Unsupported protocol version", map[string]any{"requested": 0, "supported": []int{1, 2}})
				} else if *params.ProtocolVersion == 2 {
					var v2 struct {
						Info         *acp.Implementation
						Capabilities map[string]json.RawMessage
					}
					if json.Unmarshal(frame.Params, &v2) != nil || v2.Info == nil || v2.Info.Name == "" || v2.Info.Version == "" {
						failure = acp.NewInvalidParams("invalid v2 initialization")
					} else {
						return line, 2, nil
					}
				} else {
					return line, min(*params.ProtocolVersion, 2), nil
				}
			}
			if failure != nil {
				if err := json.NewEncoder(out).Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": failure}); err != nil {
					return nil, 0, err
				}
			}
		}
		line = line[:0]
	}
}

func (c *AgentConnection) Done() <-chan struct{} { return c.done }

func (c *AgentConnection) SetLogger(logger *slog.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logger = logger
	if c.setLogger != nil {
		c.setLogger(logger)
	}
}

func (c *AgentConnection) client(ctx context.Context) (ClientConnection, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ready:
		if c.ClientConnection == nil {
			return nil, io.ErrClosedPipe
		}
		return c.ClientConnection, nil
	}
}

func (c *AgentConnection) SessionUpdate(ctx context.Context, p acp.SessionNotification) error {
	client, err := c.client(ctx)
	if err != nil {
		return err
	}
	return client.SessionUpdate(ctx, p)
}

func (c *AgentConnection) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	client, err := c.client(ctx)
	if err != nil {
		return acp.RequestPermissionResponse{}, err
	}
	return client.RequestPermission(ctx, p)
}

func (c *AgentConnection) CreateElicitation(ctx context.Context, p acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
	client, err := c.client(ctx)
	if err != nil {
		return acp.CreateElicitationResponse{}, err
	}
	return client.CreateElicitation(ctx, p)
}

type acceptedPromptKey struct{}

// PromptAccepted is called only after the backend has taken the input. v1 has
// no acknowledgement; v2 ends the request here and streams the remaining work.
func PromptAccepted(ctx context.Context, messageID string) {
	if accepted, ok := ctx.Value(acceptedPromptKey{}).(func(string)); ok {
		accepted(messageID)
	}
}

type backgroundInteractionKey struct{}

// WithBackgroundInteraction marks an interaction that leaves the foreground turn running.
func WithBackgroundInteraction(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundInteractionKey{}, true)
}
