package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	"github.com/coder/websocket"
	"go.lsp.dev/jsonrpc2"
)

// editorState belongs to the web adapter. Headless projects have no editor
// connections or document ownership.
type editorState struct {
	lspDocumentsMu   sync.Mutex
	lspDocuments     map[string]*lspDocument
	lspExternalMu    sync.Mutex
	lspExternalPaths map[string]bool
}

// The browser transport uses one UTF-8 JSON message per WebSocket frame.
// Native language servers keep their Content-Length framing.
// Slow peers are disconnected instead of dropping protocol messages.
type lspConnection struct {
	server    *Server
	language  *language.Service
	ctx       context.Context
	cancel    context.CancelFunc
	writer    *wsClient
	mu        sync.Mutex
	pending   map[string]context.CancelCauseFunc
	documents map[string]lspBuffer
	replies   map[string]chan json.RawMessage
	sequence  atomic.Uint64
}

var (
	errEditorCancelled = errors.New("cancelled")
	errEditorTimeout   = errors.New("request timed out")
)

func (s *Server) acceptLSPConnection(w http.ResponseWriter, r *http.Request) (*lspConnection, error) {
	service := s.Project.workspace.LanguageService()
	if service == nil {
		err := errors.New("language service unavailable")
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return nil, err
	}
	// The default origin check confines /lsp to the same-origin web UI.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(8 << 20)
	ctx, cancel := context.WithCancel(s.Project.ctx)
	c := &lspConnection{
		server:    s,
		language:  service,
		ctx:       ctx,
		cancel:    cancel,
		writer:    newWSClient(conn),
		pending:   make(map[string]context.CancelCauseFunc),
		documents: make(map[string]lspBuffer),
		replies:   make(map[string]chan json.RawMessage),
	}
	go c.writer.run()
	go func() { <-ctx.Done(); _ = conn.CloseNow() }()
	return c, nil
}

func (c *lspConnection) close() {
	c.cancel()
	c.writer.close()
	_ = c.writer.conn.CloseNow()
}

func (c *lspConnection) send(value any) {
	data, err := json.Marshal(value)
	if err != nil || !c.writer.enqueue(data) {
		c.cancel()
	}
}

// begin registers a cancellable request. A zero timeout leaves the request
// bounded only by the socket and explicit cancellation.
func (c *lspConnection) begin(id string, timeout time.Duration) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.pending[id]; exists {
		return nil, nil, errors.New("duplicate request id")
	}
	if len(c.pending) >= 64 {
		return nil, nil, errors.New("too many outstanding requests")
	}
	ctx, cancel := context.WithCancelCause(c.ctx)
	stop := context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, stop = context.WithTimeoutCause(ctx, timeout, errEditorTimeout)
	}
	c.pending[id] = cancel
	return ctx, func() {
		stop()
		cancel(context.Canceled)
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}, nil
}

func (c *lspConnection) cancelRequest(id string) {
	c.mu.Lock()
	cancel := c.pending[id]
	c.mu.Unlock()
	if cancel != nil {
		cancel(errEditorCancelled)
	}
}

// editorRequestError reports why a request context ended instead of the
// generic context error a downstream call returned.
func editorRequestError(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return err
}

func (c *lspConnection) notify(method string, params any) {
	c.send(ahp.Notify(method, params))
}

func (c *lspConnection) reply(id json.RawMessage, result any, err error) {
	if len(id) == 0 {
		return
	}
	if err == nil {
		c.send(ahp.Reply(id, result, nil))
		return
	}
	rpc := &ahp.Error{Code: -32603, Message: err.Error()}
	var typed *ahp.Error
	var native *jsonrpc2.Error
	switch {
	case errors.Is(err, errEditorCancelled):
		rpc.Code = -32800
	case errors.Is(err, errEditorTimeout):
		rpc.Code = -32803
	case errors.As(err, &typed):
		rpc = typed
	case errors.As(err, &native):
		// Language server errors such as ContentModified keep their meaning.
		rpc = &ahp.Error{Code: int(native.Code), Message: native.Message}
		if len(native.Data) > 0 {
			rpc.Data = json.RawMessage(native.Data)
		}
	}
	c.send(ahp.Reply(id, nil, rpc))
}
