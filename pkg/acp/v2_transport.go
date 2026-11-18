package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
)

// The SDK dispatches requests concurrently. Preserve their arrival order for
// session/cancel and signal when setup/prompt responses have reached the wire.
type v2Wire struct {
	scanner        *bufio.Scanner
	out            io.Writer
	buffer         []byte
	sequence       uint64
	mu             sync.Mutex
	outMu          sync.Mutex
	previousPrompt <-chan struct{}
	requests       map[uint64]v2Request
	responses      map[string]v2Request
}

type v2Request struct {
	sequence       uint64
	sent           chan struct{}
	previousPrompt <-chan struct{}
	registered     func()
}
type v2RequestKey struct{}

func newV2Wire(out io.Writer, in io.Reader) *v2Wire {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(nil, 10*1024*1024)
	return &v2Wire{scanner: scanner, out: out, requests: map[uint64]v2Request{}, responses: map[string]v2Request{}}
}

func (w *v2Wire) Read(p []byte) (int, error) {
	for len(w.buffer) == 0 {
		if !w.scanner.Scan() {
			if err := w.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		line := bytes.TrimSpace(w.scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame map[string]json.RawMessage
		if !json.Valid(line) {
			if err := w.writeLine(parseErrorFrame); err != nil {
				return 0, err
			}
			continue
		}
		if json.Unmarshal(line, &frame) != nil || string(frame["jsonrpc"]) != `"2.0"` || (frame["method"] != nil && !validRequestID(frame["id"])) {
			if err := w.writeLine(invalidRequestFrame); err != nil {
				return 0, err
			}
			continue
		}
		var method string
		_ = json.Unmarshal(frame["method"], &method)
		if method != "" && method != "$/cancel_request" {
			w.sequence++
			w.mu.Lock()
			req := v2Request{sequence: w.sequence, sent: make(chan struct{})}
			if method == "session/prompt" {
				registered := make(chan struct{})
				req.previousPrompt = w.previousPrompt
				req.registered = sync.OnceFunc(func() { close(registered) })
				w.previousPrompt = registered
			}
			w.requests[w.sequence] = req
			if id := frame["id"]; id != nil {
				w.responses[v2RequestIDKey(id)] = req
			} else {
				close(req.sent)
			}

			w.mu.Unlock()
			frame["method"], _ = json.Marshal(method + "\x00" + strconv.FormatUint(w.sequence, 10))
			line, _ = json.Marshal(frame)
		}
		w.buffer = append(bytes.Clone(line), '\n')
	}
	n := copy(p, w.buffer)
	w.buffer = w.buffer[n:]
	return n, nil
}

func validRequestID(id json.RawMessage) bool {
	if id == nil || (len(id) > 0 && id[0] == '"') {
		return true
	}
	digits := bytes.TrimPrefix(id, []byte("-"))
	return len(digits) > 0 && len(bytes.Trim(digits, "0123456789")) == 0
}

var parseErrorFrame = json.RawMessage(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)
var invalidRequestFrame = json.RawMessage(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`)

// Forwarding through encoding/json can change a string ID's escape spelling.
// Compare decoded strings, while preserving numeric IDs without float rounding.
func v2RequestIDKey(id json.RawMessage) string {
	id = bytes.TrimSpace(id)
	if len(id) > 0 && id[0] == '"' {
		var value string
		if json.Unmarshal(id, &value) == nil {
			id, _ = json.Marshal(value)
		}
	}
	return string(id)
}

func (w *v2Wire) request(method string) (string, v2Request) {
	index := strings.LastIndexByte(method, 0)
	if index < 0 {
		return method, v2Request{}
	}
	sequence, _ := strconv.ParseUint(method[index+1:], 10, 64)
	w.mu.Lock()
	defer w.mu.Unlock()
	request := w.requests[sequence]
	delete(w.requests, sequence)
	return method[:index], request
}

func (w *v2Wire) Write(p []byte) (int, error) {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	var response v2Request
	if json.Unmarshal(p, &frame) == nil && frame.Method == "" && len(frame.ID) > 0 {
		w.mu.Lock()
		id := v2RequestIDKey(frame.ID)
		response = w.responses[id]
		delete(w.requests, response.sequence)
		delete(w.responses, id)
		w.mu.Unlock()
	}
	if err := w.writeLine(p); err != nil {
		return 0, err
	}
	if response.registered != nil {
		response.registered()
	}
	if response.sent != nil {
		close(response.sent)
	}
	return len(p), nil
}

func (w *v2Wire) writeLine(line []byte) error {
	line = append(bytes.TrimSpace(line), '\n')
	w.outMu.Lock()
	defer w.outMu.Unlock()
	n, err := w.out.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	return err
}
