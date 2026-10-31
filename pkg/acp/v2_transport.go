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
	scanner   *bufio.Scanner
	out       io.Writer
	buffer    []byte
	frames    []json.RawMessage
	sequence  uint64
	mu        sync.Mutex
	outMu     sync.Mutex
	requests  map[uint64]json.RawMessage
	responses map[string]chan struct{}
	batches   map[string]*v2Batch
}

type v2Batch struct {
	pending   int
	responses []json.RawMessage
	sent      []chan struct{}
}

type v2Request struct {
	sequence uint64
	sent     <-chan struct{}
}
type v2RequestKey struct{}

func newV2Wire(out io.Writer, in io.Reader) *v2Wire {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(nil, 10*1024*1024)
	return &v2Wire{scanner: scanner, out: out, requests: map[uint64]json.RawMessage{}, responses: map[string]chan struct{}{}, batches: map[string]*v2Batch{}}
}

func (w *v2Wire) Read(p []byte) (int, error) {
	for len(w.buffer) == 0 {
		if len(w.frames) == 0 {
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
			if line[0] == '[' {
				if err := w.readBatch(line); err != nil {
					return 0, err
				}
				continue
			}
			w.frames = append(w.frames, bytes.Clone(line))
		}
		line := w.frames[0]
		w.frames = w.frames[1:]
		var frame map[string]json.RawMessage
		if json.Unmarshal(line, &frame) != nil {
			w.buffer = append(bytes.Clone(line), '\n')
			break
		}
		var method string
		_ = json.Unmarshal(frame["method"], &method)
		if method != "" && method != "$/cancel_request" {
			w.sequence++
			w.mu.Lock()
			w.requests[w.sequence] = frame["id"]
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

var invalidBatchEntry = json.RawMessage(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`)

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

func (w *v2Wire) readBatch(line []byte) error {
	var entries []json.RawMessage
	if err := json.Unmarshal(line, &entries); err != nil || len(entries) == 0 {
		return w.writeLine(invalidBatchEntry)
	}
	batch := &v2Batch{}
	w.mu.Lock()
	for _, entry := range entries {
		var frame map[string]json.RawMessage
		if json.Unmarshal(entry, &frame) != nil || frame == nil || frame["method"] == nil && frame["result"] == nil && frame["error"] == nil {
			batch.responses = append(batch.responses, invalidBatchEntry)
			continue
		}
		if frame["method"] != nil && frame["id"] != nil {
			batch.pending++
			w.batches[v2RequestIDKey(frame["id"])] = batch
		}
		w.frames = append(w.frames, entry)
	}
	w.mu.Unlock()
	if batch.pending == 0 && len(batch.responses) > 0 {
		data, _ := json.Marshal(batch.responses)
		return w.writeLine(data)
	}
	return nil
}

func (w *v2Wire) request(method string) (string, v2Request) {
	index := strings.LastIndexByte(method, 0)
	if index < 0 {
		return method, v2Request{}
	}
	sequence, _ := strconv.ParseUint(method[index+1:], 10, 64)
	w.mu.Lock()
	defer w.mu.Unlock()
	id := w.requests[sequence]
	delete(w.requests, sequence)
	sent := make(chan struct{})
	if len(id) > 0 {
		w.responses[v2RequestIDKey(id)] = sent
	} else {
		close(sent)
	}
	return method[:index], v2Request{sequence: sequence, sent: sent}
}

func (w *v2Wire) Write(p []byte) (int, error) {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	var sent []chan struct{}
	line := p
	if json.Unmarshal(p, &frame) == nil && frame.Method == "" && len(frame.ID) > 0 {
		w.mu.Lock()
		id := v2RequestIDKey(frame.ID)
		if signal := w.responses[id]; signal != nil {
			sent = append(sent, signal)
			delete(w.responses, id)
		}
		if batch := w.batches[id]; batch != nil {
			delete(w.batches, id)
			batch.responses = append(batch.responses, bytes.Clone(p))
			batch.sent = append(batch.sent, sent...)
			batch.pending--
			if batch.pending > 0 {
				w.mu.Unlock()
				return len(p), nil
			}
			line, _ = json.Marshal(batch.responses)
			sent = batch.sent
		}
		w.mu.Unlock()
	}
	if err := w.writeLine(line); err != nil {
		return 0, err
	}
	for _, signal := range sent {
		close(signal)
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
