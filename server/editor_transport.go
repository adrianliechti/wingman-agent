package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"go.lsp.dev/jsonrpc2"
)

// The browser transports use one UTF-8 JSON message per WebSocket frame.
// Native language servers and debug adapters keep their Content-Length framing.
// Slow peers are disconnected instead of dropping protocol messages.
type editorSocket struct {
	ctx     context.Context
	cancel  context.CancelFunc
	writer  *wsClient
	mu      sync.Mutex
	pending map[string]context.CancelCauseFunc
}

var (
	errEditorCancelled = errors.New("cancelled")
	errEditorTimeout   = errors.New("request timed out")
)

func (s *Server) acceptEditorSocket(w http.ResponseWriter, r *http.Request) (*editorSocket, error) {
	// Only the same-origin Web UI uses /lsp and /dap; the default origin check
	// keeps other pages from driving language servers and debuggers.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(8 << 20)
	ctx, cancel := context.WithCancel(s.ctx)
	p := &editorSocket{ctx: ctx, cancel: cancel, writer: newWSClient(conn), pending: map[string]context.CancelCauseFunc{}}
	go p.writer.run()
	go func() { <-ctx.Done(); _ = conn.CloseNow() }()
	return p, nil
}

func (p *editorSocket) close() { p.cancel(); p.writer.close(); _ = p.writer.conn.CloseNow() }
func (p *editorSocket) send(value any) {
	data, err := json.Marshal(value)
	if err != nil || !p.writer.enqueue(data) {
		p.cancel()
	}
}

// begin registers a cancellable request. A zero timeout leaves the request
// bounded only by the socket and explicit cancellation.
func (p *editorSocket) begin(id string, timeout time.Duration) (context.Context, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.pending[id]; exists {
		return nil, nil, errors.New("duplicate request id")
	}
	if len(p.pending) >= 64 {
		return nil, nil, errors.New("too many outstanding requests")
	}
	ctx, cancel := context.WithCancelCause(p.ctx)
	stop := context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, stop = context.WithTimeoutCause(ctx, timeout, errEditorTimeout)
	}
	p.pending[id] = cancel
	return ctx, func() { stop(); cancel(context.Canceled); p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }, nil
}

func (p *editorSocket) cancelRequest(id string) {
	p.mu.Lock()
	cancel := p.pending[id]
	p.mu.Unlock()
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

func (p *editorSocket) notify(method string, params any) {
	p.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (p *editorSocket) reply(id json.RawMessage, result any, err error) {
	if len(id) == 0 {
		return
	}
	if err == nil {
		p.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		return
	}
	rpc := &ahpRPCError{Code: -32603, Message: err.Error()}
	var typed *ahpRPCError
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
		rpc = &ahpRPCError{Code: int(native.Code), Message: native.Message}
		if len(native.Data) > 0 {
			rpc.Data = json.RawMessage(native.Data)
		}
	}
	p.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpc})
}
