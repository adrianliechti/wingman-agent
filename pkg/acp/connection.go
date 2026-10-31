package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error)
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
}

func NewAgentConnection(agent connectedAgent, out io.Writer, in io.Reader) *AgentConnection {
	c := &AgentConnection{ready: make(chan struct{}), done: make(chan struct{})}
	agent.SetClientConnection(c)
	go func() {
		defer close(c.done)
		reader := bufio.NewReader(in)
		var first []byte
		for len(first) < 32*1024*1024 {
			part, err := reader.ReadSlice('\n')
			first = append(first, part...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				close(c.ready)
				return
			}
			if len(bytes.TrimSpace(first)) == 0 {
				first = first[:0]
				continue
			}
			break
		}
		if len(first) >= 32*1024*1024 {
			close(c.ready)
			return
		}
		var frame struct {
			Method string `json:"method"`
			Params struct {
				ProtocolVersion int `json:"protocolVersion"`
			} `json:"params"`
		}
		_ = json.Unmarshal(first, &frame)
		input := io.MultiReader(bytes.NewReader(first), reader)
		c.mu.Lock()
		if frame.Method == "initialize" && frame.Params.ProtocolVersion != 1 {
			v2 := newV2Connection(agent, out, input, c.ready)
			c.ClientConnection = v2
			c.setLogger = v2.rpc.SetLogger
		} else {
			v1 := acp.NewAgentSideConnection(agent, out, input)
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

func (c *AgentConnection) UnstableCreateElicitation(ctx context.Context, p acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	client, err := c.client(ctx)
	if err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}
	return client.UnstableCreateElicitation(ctx, p)
}

type acceptedPromptKey struct{}

// PromptAccepted is called only after the backend has taken the input. v1 has
// no acknowledgement; v2 ends the request here and streams the remaining work.
func PromptAccepted(ctx context.Context, messageID string) {
	if accepted, ok := ctx.Value(acceptedPromptKey{}).(func(string)); ok {
		accepted(messageID)
	}
}
