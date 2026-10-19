package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

const (
	maxQueuedTurnEvents = 64 * 1024
	maxQueuedTurnBytes  = 32 * 1024 * 1024
)

// Only enqueue on the RPC reader. The prompt consumes notifications in wire
// order after turn/start identifies their owner; client writes and approvals
// must never prevent the reader from receiving control replies or EOF.
type turnStream struct {
	ctx    context.Context
	mu     sync.Mutex
	events []turnEvent
	signal chan struct{}
	bytes  int // Queued payload size; protected with events by mu.
	failed chan error
	ready  chan struct{}
	turnID string // published by closing ready
}

type turnEvent struct {
	rpcMessage
	processed chan struct{}
}

func newTurnStream(ctx context.Context) *turnStream {
	return &turnStream{
		ctx: ctx, signal: make(chan struct{}, 1),
		failed: make(chan error, 1), ready: make(chan struct{}),
	}
}

func (s *turnStream) enqueue(method string, params json.RawMessage) {
	if s.ctx.Err() != nil {
		return
	}
	if s.push(turnEvent{rpcMessage: rpcMessage{Method: method, Params: params}}) {
		return
	}
	select {
	case s.failed <- fmt.Errorf("Codex updates exceeded the client delivery queue"):
	default:
	}
}

// push bounds provider updates while always admitting internal delivery barriers.
func (s *turnStream) push(event turnEvent) bool {
	s.mu.Lock()
	size := len(event.Method) + len(event.Params)
	if event.processed == nil && (len(s.events) >= maxQueuedTurnEvents || s.bytes+size > maxQueuedTurnBytes) {
		s.mu.Unlock()
		return false
	}
	s.events = append(s.events, event)
	s.bytes += size
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
	return true
}

func (s *turnStream) pop() (turnEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return turnEvent{}, false
	}
	event := s.events[0]
	s.bytes -= len(event.Method) + len(event.Params)
	s.events[0] = turnEvent{}
	s.events = s.events[1:]
	if len(s.events) == 0 {
		s.events = nil
	}
	return event, true
}

func (s *turnStream) started(turnID string) {
	s.turnID = turnID
	close(s.ready)
}

func (s *turnStream) acceptsRequest(ctx context.Context, turnID string) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.ready:
		if ctx.Err() != nil || (turnID != "" && turnID != s.turnID) {
			return false
		}
	}
	// Flush earlier tool-start updates before opening a permission dialog.
	// Only the request goroutine waits; the RPC reader must remain available.
	processed := make(chan struct{})
	s.push(turnEvent{processed: processed})
	select {
	case <-processed:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func (s *turnStream) owns(event rpcMessage) (bool, error) {
	var p struct {
		TurnID string `json:"turnId"`
		Turn   *struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(event.Params, &p); err != nil {
		return false, fmt.Errorf("decode %s scope: %w", event.Method, err)
	}
	if event.Method == "turn/completed" && (p.Turn == nil || p.Turn.ID == "") {
		return false, fmt.Errorf("turn/completed is missing its turn id")
	}
	if p.Turn != nil {
		return p.Turn.ID == s.turnID, nil
	}
	return p.TurnID == "" || p.TurnID == s.turnID, nil
}

func (s *turnStream) next(rpc *rpcClient) (turnEvent, error) {
	var event turnEvent
	select {
	case err := <-s.failed:
		return event, err
	default:
	}
	select {
	case <-s.ctx.Done():
		return event, s.ctx.Err()
	default:
	}
	// Drain already-read events before EOF, including a final turn/completed.
	for {
		if event, ok := s.pop(); ok {
			return event, nil
		}
		select {
		case <-s.signal:
		case <-s.ctx.Done():
			return event, s.ctx.Err()
		case err := <-s.failed:
			return event, err
		case <-rpc.done:
			if event, ok := s.pop(); ok {
				return event, nil
			}
			return event, rpc.closedError()
		}
	}
}
