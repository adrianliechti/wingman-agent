package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// rpcMessage is a minimal JSON-RPC 2.0 envelope. We tolerate codex's habit of
// omitting the `jsonrpc` field on both inbound and outbound messages: marshal
// always emits "2.0", unmarshal accepts its absence.
type rpcMessage struct {
	Jsonrpc string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// rpcClient speaks newline-delimited JSON-RPC 2.0 over a duplex stream. It
// drives `codex app-server` from a single goroutine: writes are serialized by
// a mutex, reads happen on a background loop that dispatches responses to
// pending callers, notifications to onNotification, and inbound requests to
// onRequest. When the read loop exits (codex died, EOF), `done` closes so
// pending calls bail instead of hanging.
type rpcClient struct {
	w  io.Writer
	r  io.Reader
	mu sync.Mutex // protects writes

	nextID  atomic.Int64
	pending sync.Map // map[string]chan rpcMessage

	onNotification func(method string, params json.RawMessage)
	onRequest      func(ctx context.Context, method string, params json.RawMessage) (any, *rpcError)

	done chan struct{}
}

func newRPCClient(w io.Writer, r io.Reader) *rpcClient {
	return &rpcClient{w: w, r: r, done: make(chan struct{})}
}

func (c *rpcClient) start() {
	go c.readLoop()
}

func (c *rpcClient) readLoop() {
	defer close(c.done)
	scanner := bufio.NewScanner(c.r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			c.dispatchRequest(msg)
		case msg.Method != "":
			if c.onNotification != nil {
				c.onNotification(msg.Method, msg.Params)
			}
		case len(msg.ID) > 0:
			key := string(msg.ID)
			if ch, ok := c.pending.LoadAndDelete(key); ok {
				ch.(chan rpcMessage) <- msg
			}
		}
	}
}

func (c *rpcClient) dispatchRequest(msg rpcMessage) {
	go func() {
		var (
			result any
			rerr   *rpcError
		)
		if c.onRequest != nil {
			result, rerr = c.onRequest(context.Background(), msg.Method, msg.Params)
		} else {
			rerr = &rpcError{Code: -32601, Message: "method not found"}
		}
		resp := rpcMessage{Jsonrpc: "2.0", ID: msg.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			b, err := json.Marshal(result)
			if err != nil {
				resp.Error = &rpcError{Code: -32603, Message: err.Error()}
			} else {
				resp.Result = b
			}
		}
		_ = c.send(resp)
	}()
}

var errRPCClosed = fmt.Errorf("codex app-server connection closed")

func (c *rpcClient) send(msg rpcMessage) error {
	select {
	case <-c.done:
		return errRPCClosed
	default:
	}
	msg.Jsonrpc = "2.0"
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// call sends a JSON-RPC request and waits for the response. params may be nil.
func (c *rpcClient) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	idRaw, _ := json.Marshal(id)
	ch := make(chan rpcMessage, 1)
	c.pending.Store(string(idRaw), ch)

	req := rpcMessage{ID: idRaw, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			c.pending.Delete(string(idRaw))
			return err
		}
		req.Params = b
	}
	if err := c.send(req); err != nil {
		c.pending.Delete(string(idRaw))
		return err
	}

	select {
	case <-ctx.Done():
		c.pending.Delete(string(idRaw))
		return ctx.Err()
	case <-c.done:
		c.pending.Delete(string(idRaw))
		return errRPCClosed
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}
