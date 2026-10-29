package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The browser transports use one UTF-8 JSON message per WebSocket frame.
// Native language servers and debug adapters keep their Content-Length framing.
// Slow peers are disconnected instead of dropping protocol messages.
type editorSocket struct {
	ctx     context.Context
	cancel  context.CancelFunc
	writer  *wsClient
	mu      sync.Mutex
	pending map[string]context.CancelFunc
}

func (s *Server) acceptEditorSocket(w http.ResponseWriter, r *http.Request) (*editorSocket, error) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(8 << 20)
	ctx, cancel := context.WithCancel(s.ctx)
	p := &editorSocket{ctx: ctx, cancel: cancel, writer: newWSClient(conn), pending: map[string]context.CancelFunc{}}
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

func (p *editorSocket) begin(id string) (context.Context, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.pending[id]; exists {
		return nil, nil, errors.New("duplicate request id")
	}
	if len(p.pending) >= 64 {
		return nil, nil, errors.New("too many outstanding requests")
	}
	ctx, cancel := context.WithTimeout(p.ctx, 2*time.Minute)
	p.pending[id] = cancel
	return ctx, func() { cancel(); p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }, nil
}

func (p *editorSocket) cancelRequest(id string) {
	p.mu.Lock()
	cancel := p.pending[id]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *editorSocket) notify(method string, params any) {
	p.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (p *editorSocket) reply(id json.RawMessage, result any, err error) {
	if len(id) == 0 {
		return
	}
	if err != nil {
		rpc := &ahpRPCError{Code: -32603, Message: err.Error()}
		var typed *ahpRPCError
		if errors.As(err, &typed) {
			rpc = typed
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			rpc.Code = -32800
		}
		p.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpc})
	} else {
		p.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
}
