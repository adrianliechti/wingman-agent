package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
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
	bytes  atomic.Int64
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
	size := int64(len(method) + len(params))
	if s.bytes.Add(size) <= maxQueuedTurnBytes && s.push(turnEvent{rpcMessage: rpcMessage{Method: method, Params: params}}, maxQueuedTurnEvents) {
		return
	}
	s.bytes.Add(-size)
	select {
	case s.failed <- fmt.Errorf("Codex updates exceeded the client delivery queue"):
	default:
	}
}

// push appends unless the queue already holds limit events; limit < 0 means unbounded.
func (s *turnStream) push(event turnEvent, limit int) bool {
	s.mu.Lock()
	if limit >= 0 && len(s.events) >= limit {
		s.mu.Unlock()
		return false
	}
	s.events = append(s.events, event)
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
	s.events[0] = turnEvent{}
	s.events = s.events[1:]
	if len(s.events) == 0 {
		s.events = nil
	}
	return event, true
}

func (s *turnStream) queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
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
	s.push(turnEvent{processed: processed}, -1)
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
			s.bytes.Add(-int64(len(event.Method) + len(event.Params)))
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
				s.bytes.Add(-int64(len(event.Method) + len(event.Params)))
				return event, nil
			}
			return event, rpc.closedError()
		}
	}
}
