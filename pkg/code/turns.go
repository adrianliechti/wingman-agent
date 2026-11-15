package code

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

type managedTurnInput struct {
	input      TurnInput
	terminal   TurnInputState
	err        error
	delivering bool // steering handed to the agent, which no edit may change
}

type managedTurnSession struct {
	mu          sync.Mutex
	queueLoaded bool
	queueErr    error
	done        chan struct{}

	active  *managedTurnInput
	queued  []*managedTurnInput
	steered []*managedTurnInput

	cancel          context.CancelCauseFunc
	cancelRequested bool
	paused          bool
	running         bool
	compacting      bool
	ids             map[string]struct{}
}

// TurnManager provides the session-level execution semantics shared by the
// web UI and other multi-turn clients. It guarantees one Agent.Send call per
// session at a time, supplies a FIFO follow-up queue for every backend, and
// uses native steering only when the backend explicitly supports it.
type TurnManager struct {
	ctx    context.Context
	cancel context.CancelFunc
	agent  Agent

	handlerMu sync.RWMutex
	handler   func(TurnEvent)

	mu       sync.Mutex
	workers  sync.WaitGroup
	sessions map[string]*managedTurnSession
}

func NewTurnManager(ctx context.Context, a Agent, handler func(TurnEvent)) *TurnManager {
	ctx, cancel := context.WithCancel(ctx)
	return &TurnManager{
		ctx: ctx, cancel: cancel, agent: a, handler: handler,
		sessions: make(map[string]*managedTurnSession),
	}
}

func (m *TurnManager) SetHandler(handler func(TurnEvent)) {
	m.handlerMu.Lock()
	m.handler = handler
	m.handlerMu.Unlock()
}

func (m *TurnManager) emit(ev TurnEvent) {
	m.handlerMu.RLock()
	h := m.handler
	m.handlerMu.RUnlock()
	if h != nil {
		h(ev)
	}
}

func (m *TurnManager) session(id string) *managedTurnSession {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		s = &managedTurnSession{}
		m.sessions[id] = s
	}
	m.mu.Unlock()
	// Loading one session must never hold the workspace-wide registry lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queueLoaded {
		return s
	}
	store, ok := m.agent.(TurnQueueStore)
	if !ok {
		s.queueLoaded = true
		return s
	}
	state, err := store.LoadTurnQueue(id)
	if err != nil {
		s.queueErr = fmt.Errorf("restore turn queue: %w", err)
		return s
	}
	s.queueLoaded, s.queueErr = true, nil
	s.ids = make(map[string]struct{}, len(state.Inputs))
	for _, input := range state.Inputs {
		if input.ID == "" {
			continue
		}
		if _, duplicate := s.ids[input.ID]; duplicate {
			continue
		}
		input = CloneTurnInput(input)
		if input.Intent == "" {
			input.Intent = TurnInputFollowUp
		}
		s.ids[input.ID] = struct{}{}
		s.queued = append(s.queued, &managedTurnInput{input: input})
	}
	// Reading a recovered queue never authorizes execution.
	s.paused = runnable(s.queued) >= 0
	return s
}

// waits reports whether item is steering that waits in the queue for a turn.
func waits(item *managedTurnInput) bool { return item.input.Intent == TurnInputSteer }

// runnable is the index of the first queued input that may start a turn.
func runnable(queued []*managedTurnInput) int {
	return slices.IndexFunc(queued, func(item *managedTurnInput) bool { return !waits(item) })
}

// Admission and worker registration share the shutdown barrier. Close waits for
// accepted submissions, including their callbacks and native steering calls.
func (m *TurnManager) beginWork() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return false
	}
	m.workers.Add(1)
	return true
}

func (m *TurnManager) persistLocked(sessionID string, s *managedTurnSession) error {
	store, ok := m.agent.(TurnQueueStore)
	if !ok {
		return nil
	}
	state := TurnQueueState{Paused: s.paused, Inputs: make([]TurnInput, 0, len(s.queued))}
	for _, item := range s.queued {
		input := item.input
		input = CloneTurnInput(input)
		state.Inputs = append(state.Inputs, input)
	}
	if err := store.SaveTurnQueue(sessionID, state); err != nil {
		s.queueErr = fmt.Errorf("persist turn queue: %w", err)
		return s.queueErr
	}
	s.queueErr = nil
	return nil
}

func (m *TurnManager) Features(sessionID string) TurnFeatures {
	f := TurnFeatures{}
	if p, ok := m.agent.(TurnFeatureProvider); ok {
		provided := p.TurnFeatures(sessionID)
		f.Steer = provided.Steer
	}
	if _, ok := m.agent.(TurnSteerer); !ok {
		f.Steer = false
	}
	return f
}

// Submit rejects duplicate live input IDs. Transport receipts own retry
// deduplication. A steer that loses a turn-boundary race becomes a FIFO follow-up.
func (m *TurnManager) Submit(ctx context.Context, sessionID string, input TurnInput) (TurnInputSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return TurnInputSnapshot{}, err
	}
	if !m.beginWork() {
		return TurnInputSnapshot{}, m.ctx.Err()
	}
	defer m.workers.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	defer func() { stop(); cancel() }()
	if sessionID == "" {
		return TurnInputSnapshot{}, errors.New("session id required")
	}
	if input.ID == "" {
		return TurnInputSnapshot{}, errors.New("input id required")
	}
	if len(input.Content) == 0 && !input.Compact {
		return TurnInputSnapshot{}, errors.New("input content required")
	}
	if input.Compact {
		input.Intent = TurnInputFollowUp
	}
	switch input.Intent {
	case "":
		input.Intent = TurnInputFollowUp
	case TurnInputFollowUp, TurnInputSteer:
	default:
		return TurnInputSnapshot{}, fmt.Errorf("%w: %q", ErrInvalidIntent, input.Intent)
	}
	input = CloneTurnInput(input)

	s := m.session(sessionID)
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return TurnInputSnapshot{}, err
	}
	if !s.queueLoaded || m.ctx.Err() != nil {
		err := s.queueErr
		if m.ctx.Err() != nil {
			err = m.ctx.Err()
		}
		s.mu.Unlock()
		return TurnInputSnapshot{}, err
	}
	if s.ids == nil {
		s.ids = make(map[string]struct{})
	}
	if _, exists := s.ids[input.ID]; exists {
		s.mu.Unlock()
		return TurnInputSnapshot{}, ErrDuplicateInput
	}
	s.ids[input.ID] = struct{}{}
	s.mu.Unlock()
	steerer, _ := m.agent.(TurnSteerer)
	steerable := m.Features(sessionID).Steer
	item := &managedTurnInput{input: input}
	if input.Intent == TurnInputSteer && steerable {
		s.mu.Lock()
		target := s.active
		hasActive := target != nil && !s.cancelRequested
		s.mu.Unlock()
		if hasActive {
			state, err := m.steer(ctx, sessionID, s, steerer, target, item, input)
			switch {
			case err == nil:
				return TurnInputSnapshot{ID: input.ID, State: state, Intent: input.Intent}, nil
			case errors.Is(err, ErrNoActiveTurn) && input.Wait:
			case errors.Is(err, ErrNoActiveTurn) || errors.Is(err, ErrTurnNotSteerable):
				// Turn-boundary races and explicitly non-steerable turn kinds are
				// safe to preserve as FIFO follow-ups.
				item.input.Intent = TurnInputFollowUp
			default:
				m.releaseID(s, input.ID)
				return TurnInputSnapshot{}, fmt.Errorf("steer active turn: %w", err)
			}
		}
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		delete(s.ids, input.ID)
		s.mu.Unlock()
		return TurnInputSnapshot{}, err
	}
	if err := m.ctx.Err(); err != nil {
		delete(s.ids, input.ID)
		s.mu.Unlock()
		return TurnInputSnapshot{}, err
	}
	// A steer that could not join a turn is a follow-up from here on, unless it
	// waits for one; a turn that cannot be steered answers it after itself.
	if waits(item) && (!input.Wait || !steerable && s.active != nil) {
		item.input.Intent = TurnInputFollowUp
	}
	input = item.input
	if s.active == nil && !s.running && !s.paused && !waits(item) {
		s.active = item
		s.running = true
		s.done = make(chan struct{})
		s.cancelRequested = false
		s.mu.Unlock()
		m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(input), InputID: input.ID, State: TurnInputActive, Intent: input.Intent})
		m.startSession(sessionID, s)
		return TurnInputSnapshot{ID: input.ID, State: TurnInputActive, Intent: input.Intent}, nil
	}
	s.queued = append(s.queued, item)
	position := len(s.queued)
	if err := m.persistLocked(sessionID, s); err != nil {
		s.queued = s.queued[:len(s.queued)-1]
		delete(s.ids, input.ID)
		s.mu.Unlock()
		return TurnInputSnapshot{}, err
	}
	s.mu.Unlock()
	m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(input), InputID: input.ID, State: TurnInputQueued, Intent: input.Intent, Position: position})
	return TurnInputSnapshot{ID: input.ID, State: TurnInputQueued, Intent: input.Intent, Position: position}, nil
}

// steer delivers a copy of item's input, taken with item reserved, to target's
// running turn. Steering the agent took leaves the queue and joins that turn, or
// shares its outcome if it ended meanwhile, whatever happened to ctx since.
func (m *TurnManager) steer(ctx context.Context, sessionID string, s *managedTurnSession, steerer TurnSteerer, target, item *managedTurnInput, input TurnInput) (TurnInputState, error) {
	delivered := input
	delivered.Content = input.content() // the running turn reads the context with the input
	err := callSteer(ctx, steerer, sessionID, delivered)
	s.mu.Lock()
	item.delivering = false
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	var persistErr error
	// Removing or clearing the queue meanwhile leaves it nothing to give up.
	if i := slices.Index(s.queued, item); i >= 0 {
		s.queued = slices.Delete(s.queued, i, i+1)
		persistErr = m.persistLocked(sessionID, s)
	}
	state, runErr := TurnInputSteered, error(nil)
	if s.active == target {
		s.steered = append(s.steered, item)
	} else {
		// The turn ended after the agent took the input; re-queueing it would deliver it twice.
		state, runErr = target.terminal, target.err
		delete(s.ids, input.ID)
	}
	s.mu.Unlock()
	m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(input), InputID: input.ID, State: TurnInputSteered, Intent: input.Intent})
	if state != TurnInputSteered {
		m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(input), InputID: input.ID, State: state, Intent: input.Intent, Err: runErr})
	}
	if persistErr != nil {
		m.emit(TurnEvent{SessionID: sessionID, State: TurnInputFailed, Err: persistErr})
	}
	return state, nil
}

// steerWaiting hands the running turn the steering that waits in the queue.
// Steering its agent cannot take yet waits for the turn's next message.
func (m *TurnManager) steerWaiting(ctx context.Context, sessionID string, s *managedTurnSession) {
	steerer, _ := m.agent.(TurnSteerer)
	steerable := steerer != nil && m.Features(sessionID).Steer
	for {
		s.mu.Lock()
		i := slices.IndexFunc(s.queued, waits)
		if i < 0 || s.cancelRequested {
			s.mu.Unlock()
			return
		}
		item, target := s.queued[i], s.active
		item.delivering = steerable
		input := CloneTurnInput(item.input)
		s.mu.Unlock()
		err := ErrTurnNotSteerable
		if steerable {
			if _, err = m.steer(ctx, sessionID, s, steerer, target, item, input); err == nil {
				continue
			}
		}
		if errors.Is(err, ErrNoActiveTurn) || ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		var persistErr error
		if i = slices.Index(s.queued, item); i >= 0 {
			if errors.Is(err, ErrTurnNotSteerable) {
				item.input.Intent = TurnInputFollowUp // a turn that cannot be steered answers it after itself
			} else {
				s.queued = slices.Delete(s.queued, i, i+1)
				delete(s.ids, item.input.ID)
			}
			persistErr = m.persistLocked(sessionID, s)
		}
		input = CloneTurnInput(item.input)
		s.mu.Unlock()
		switch {
		case i < 0:
		case errors.Is(err, ErrTurnNotSteerable):
			m.emit(TurnEvent{SessionID: sessionID, Input: input, InputID: input.ID, State: TurnInputQueued, Intent: input.Intent, Position: i + 1})
		default:
			m.emit(TurnEvent{SessionID: sessionID, Input: input, InputID: input.ID, State: TurnInputFailed, Intent: input.Intent, Err: fmt.Errorf("steer active turn: %w", err)})
		}
		if persistErr != nil {
			m.emit(TurnEvent{SessionID: sessionID, State: TurnInputFailed, Err: persistErr})
		}
	}
}

func callSteer(ctx context.Context, steerer TurnSteerer, sessionID string, input TurnInput) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("agent steer panicked: %v", recovered)
		}
	}()
	return steerer.Steer(ctx, sessionID, input)
}

func (m *TurnManager) startSession(sessionID string, s *managedTurnSession) {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	// Submit/Resume hold a work reservation until after this registration. Even
	// a cancelled admission runs its terminal cleanup before Close returns.
	m.workers.Go(func() { defer close(done); m.runSession(sessionID, s) })
}

func (m *TurnManager) runSession(sessionID string, s *managedTurnSession) {
	for {
		s.mu.Lock()
		item := s.active
		if item == nil {
			s.running = false
			s.cancel = nil
			s.mu.Unlock()
			return
		}
		runCtx, cancel := context.WithCancelCause(WithSessionID(m.ctx, sessionID))
		s.cancel = cancel
		cancelBeforeStart := s.cancelRequested || m.ctx.Err() != nil
		s.mu.Unlock()

		var runErr error
		if cancelBeforeStart {
			cancel(nil)
			runErr = context.Canceled
		} else {
			runErr = m.executeInput(runCtx, sessionID, s, item)
		}
		cancel(nil)

		s.mu.Lock()
		cancelled := s.cancelRequested || errors.Is(runErr, context.Canceled)
		s.cancelRequested = false
		s.cancel = nil
		steered := append([]*managedTurnInput(nil), s.steered...)
		s.steered = nil
		s.active = nil
		delete(s.ids, item.input.ID)
		for _, in := range steered {
			delete(s.ids, in.input.ID)
		}

		state := TurnInputCompleted
		if cancelled {
			state = TurnInputCancelled
		} else if errors.Is(runErr, harness.ErrTurnIncomplete) {
			state = TurnInputIncomplete
		} else if runErr != nil {
			state = TurnInputFailed
		}
		item.terminal, item.err = state, runErr

		next, persistErr := m.promoteLocked(sessionID, s)
		// Keep running true until terminal callbacks have projected the finished
		// turn. New submits during finalization remain queued behind that boundary.
		s.mu.Unlock()
		m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(item.input), InputID: item.input.ID, State: state, Intent: item.input.Intent, Err: runErr, Executed: true})
		for _, in := range steered {
			m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(in.input), InputID: in.input.ID, State: state, Intent: in.input.Intent, Err: runErr})
		}
		s.mu.Lock()
		if next == nil && persistErr == nil {
			next, persistErr = m.promoteLocked(sessionID, s)
		}
		s.running = next != nil
		s.mu.Unlock()
		if persistErr != nil {
			m.emit(TurnEvent{SessionID: sessionID, State: TurnInputFailed, Err: persistErr})
		}
		if next == nil {
			return
		}
		m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(next.input), InputID: next.input.ID, State: TurnInputActive, Intent: next.input.Intent})
	}
}

func (m *TurnManager) executeInput(ctx context.Context, sessionID string, s *managedTurnSession, item *managedTurnInput) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("agent turn panicked: %v", recovered)
		}
	}()
	if item.input.Compact {
		compactor, ok := m.agent.(Compactor)
		if !ok {
			return errors.ErrUnsupported
		}
		var focus []string
		for _, content := range item.input.Content {
			focus = append(focus, content.AsText())
		}
		result, err := compactor.Compact(ctx, sessionID, strings.Join(focus, "\n"))
		if err == nil {
			m.emit(TurnEvent{SessionID: sessionID, InputID: item.input.ID, Compaction: &result})
		}
		return err
	}
	steerCtx := ctx
	ctx = harness.WithInputID(ctx, item.input.ID)
	ctx = harness.WithStreamEventHandlers(ctx, harness.StreamEventHandlers{
		Retry: func(info harness.RetryInfo) {
			m.emit(TurnEvent{SessionID: sessionID, InputID: item.input.ID, Retry: &info})
		},
		Reset: func() {
			m.emit(TurnEvent{SessionID: sessionID, InputID: item.input.ID, StreamEvent: harness.StreamEventReset})
		},
		Commit: func() {
			m.emit(TurnEvent{SessionID: sessionID, InputID: item.input.ID, StreamEvent: harness.StreamEventCommit})
		},
	})
	ctx = harness.WithInputContext(ctx, item.input.Context)
	stream, err := m.agent.Send(ctx, sessionID, item.input.Content)
	if err != nil {
		return err
	}
	if stream == nil {
		return errors.New("agent returned a nil turn stream")
	}
	m.steerWaiting(steerCtx, sessionID, s)
	for msg, streamErr := range stream {
		if streamErr != nil {
			return streamErr
		}
		copy := msg
		m.emit(TurnEvent{SessionID: sessionID, InputID: item.input.ID, Message: &copy})
		m.steerWaiting(steerCtx, sessionID, s)
	}
	return nil
}

// StartCompact runs compaction as the session's exclusive operation; inputs queue behind it.
func (m *TurnManager) StartCompact(sessionID, focus string, done func(harness.CompactResult, error)) error {
	compactor, ok := m.agent.(Compactor)
	if !ok {
		return errors.ErrUnsupported
	}
	if !m.beginWork() {
		return m.ctx.Err()
	}
	defer m.workers.Done()
	s := m.session(sessionID)
	s.mu.Lock()
	if !s.queueLoaded {
		err := s.queueErr
		s.mu.Unlock()
		return err
	}
	if s.active != nil || s.running {
		s.mu.Unlock()
		return ErrTurnInProgress
	}
	ctx, cancel := context.WithCancelCause(WithSessionID(m.ctx, sessionID))
	s.running, s.compacting = true, true
	s.cancel, s.cancelRequested = cancel, false
	s.done = make(chan struct{})
	finished := s.done
	s.mu.Unlock()

	m.workers.Go(func() {
		defer close(finished)
		result, err := compactor.Compact(ctx, sessionID, focus)
		cancel(nil)

		s.mu.Lock()
		s.compacting = false
		s.cancel, s.cancelRequested = nil, false
		next, persistErr := m.promoteLocked(sessionID, s)
		// Keep the session reserved until the completion callback has published
		// its result, just as runSession does for a finished turn.
		s.mu.Unlock()

		done(result, err)
		s.mu.Lock()
		if next == nil && persistErr == nil {
			next, persistErr = m.promoteLocked(sessionID, s)
		}
		s.running = next != nil
		s.mu.Unlock()
		if persistErr != nil {
			m.emit(TurnEvent{SessionID: sessionID, State: TurnInputFailed, Err: persistErr})
		}
		if next == nil {
			return
		}
		m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(next.input), InputID: next.input.ID, State: TurnInputActive, Intent: next.input.Intent})
		m.runSession(sessionID, s)
	})
	return nil
}

// promoteLocked activates the next queued input unless paused and persists the queue.
func (m *TurnManager) promoteLocked(sessionID string, s *managedTurnSession) (*managedTurnInput, error) {
	var next *managedTurnInput
	i := runnable(s.queued)
	if m.ctx.Err() == nil && !s.paused && i >= 0 {
		next = s.queued[i]
		s.queued = slices.Delete(s.queued, i, i+1)
		s.active = next
	}
	if err := m.persistLocked(sessionID, s); err != nil {
		if next != nil {
			s.active = nil
			s.queued = slices.Insert(s.queued, i, next)
		}
		s.paused = len(s.queued) > 0
		s.queueErr = err
		return nil, err
	}
	return next, nil
}

func (m *TurnManager) RemoveQueued(sessionID, inputID string) error {
	s := m.session(sessionID)
	s.mu.Lock()
	if !s.queueLoaded {
		err := s.queueErr
		s.mu.Unlock()
		return err
	}
	idx := -1
	var removed *managedTurnInput
	for i, item := range s.queued {
		if item.input.ID == inputID && !item.delivering {
			idx = i
			removed = item
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return ErrInputNotQueued
	}
	previousPaused := s.paused
	s.queued = append(s.queued[:idx], s.queued[idx+1:]...)
	delete(s.ids, inputID)
	if len(s.queued) == 0 {
		s.paused = false
	}
	if err := m.persistLocked(sessionID, s); err != nil {
		s.queued = append(s.queued, nil)
		copy(s.queued[idx+1:], s.queued[idx:])
		s.queued[idx] = removed
		s.ids[inputID] = struct{}{}
		s.paused = previousPaused
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(removed.input), InputID: inputID, State: TurnInputCancelled, Intent: removed.input.Intent})
	m.emitQueuePositions(sessionID, s)
	return nil
}

// ReorderQueued moves the named inputs to the head of the queue in the given
// order. Inputs it does not name follow in the order they had.
func (m *TurnManager) ReorderQueued(sessionID string, order []string) error {
	s := m.session(sessionID)
	s.mu.Lock()
	if !s.queueLoaded {
		err := s.queueErr
		s.mu.Unlock()
		return err
	}
	previous := s.queued
	reordered := make([]*managedTurnInput, 0, len(previous))
	for _, id := range order {
		i := slices.IndexFunc(previous, func(item *managedTurnInput) bool { return item.input.ID == id })
		if i >= 0 && !slices.Contains(reordered, previous[i]) {
			reordered = append(reordered, previous[i])
		}
	}
	for _, item := range previous {
		if !slices.Contains(reordered, item) {
			reordered = append(reordered, item)
		}
	}
	s.queued = reordered
	if err := m.persistLocked(sessionID, s); err != nil {
		s.queued = previous
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	m.emitQueuePositions(sessionID, s)
	return nil
}

func (m *TurnManager) ReplaceQueued(sessionID, inputID string, replacement TurnInput) error {
	if len(replacement.Content) == 0 && !replacement.Compact {
		return errors.New("input content required")
	}
	s := m.session(sessionID)
	s.mu.Lock()
	if !s.queueLoaded {
		err := s.queueErr
		s.mu.Unlock()
		return err
	}
	position := 0
	var previous TurnInput
	// Only steering that asks to wait keeps waiting for a turn; any other replacement is a follow-up.
	replacement.ID = inputID
	if replacement.Intent != TurnInputSteer || !replacement.Wait {
		replacement.Intent, replacement.Wait = TurnInputFollowUp, false
	}
	replacement = CloneTurnInput(replacement)
	start := false
	for i, item := range s.queued {
		if item.input.ID != inputID || item.delivering {
			continue
		}
		// Steering that waited while no turn ran starts one as a follow-up.
		start = waits(item) && !replacement.Wait && !s.paused
		previous = item.input
		item.input = replacement
		position = i + 1
		break
	}
	if position == 0 {
		s.mu.Unlock()
		return ErrInputNotQueued
	}
	if err := m.persistLocked(sessionID, s); err != nil {
		s.queued[position-1].input = previous
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(replacement), InputID: inputID, State: TurnInputQueued, Intent: replacement.Intent, Position: position})
	if start {
		m.Resume(sessionID)
	}
	return nil
}

// CancelCurrent interrupts the active turn and pauses queued follow-ups.
func (m *TurnManager) CancelCurrent(sessionID string) error {
	return m.cancelSession(sessionID, "", false)
}

// CancelAll interrupts the active turn and cancels every queued follow-up.
func (m *TurnManager) CancelAll(sessionID string) error {
	return m.cancelSession(sessionID, "", true)
}

// CancelTurn interrupts inputID's turn like CancelCurrent or CancelAll, and
// changes nothing with ErrNoActiveTurn once that turn is no longer active.
func (m *TurnManager) CancelTurn(sessionID, inputID string, clearQueue bool) error {
	if inputID == "" {
		return errors.New("input id required")
	}
	return m.cancelSession(sessionID, inputID, clearQueue)
}

func (m *TurnManager) cancelSession(sessionID, inputID string, clearQueue bool) error {
	s := m.session(sessionID)
	s.mu.Lock()
	if !s.queueLoaded {
		err := s.queueErr
		s.mu.Unlock()
		return err
	}
	if inputID != "" && (s.active == nil || s.active.input.ID != inputID) {
		s.mu.Unlock()
		return ErrNoActiveTurn
	}
	previousQueue, previousPaused, previousCancelled := s.queued, s.paused, s.cancelRequested
	s.cancelRequested = s.active != nil
	if !clearQueue && runnable(s.queued) >= 0 {
		s.paused = true
	}
	queued := []*managedTurnInput(nil)
	if clearQueue {
		queued = append(queued, s.queued...)
		for _, item := range queued {
			delete(s.ids, item.input.ID)
		}
		s.queued = nil
		s.paused = false
	}
	if err := m.persistLocked(sessionID, s); err != nil {
		s.queued = previousQueue
		s.paused = previousPaused
		s.cancelRequested = previousCancelled
		for _, item := range previousQueue {
			s.ids[item.input.ID] = struct{}{}
		}
		s.mu.Unlock()
		return err
	}
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel(harness.ErrInterrupted)
	}
	m.agent.Cancel(sessionID)
	for _, item := range queued {
		m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(item.input), InputID: item.input.ID, State: TurnInputCancelled, Intent: item.input.Intent})
	}
	return nil
}

func (m *TurnManager) ClearQueue(sessionID string) error {
	s := m.session(sessionID)
	s.mu.Lock()
	if !s.queueLoaded {
		err := s.queueErr
		s.mu.Unlock()
		return err
	}
	previousPaused := s.paused
	queued := append([]*managedTurnInput(nil), s.queued...)
	for _, item := range queued {
		delete(s.ids, item.input.ID)
	}
	s.queued = nil
	s.paused = false
	if err := m.persistLocked(sessionID, s); err != nil {
		s.queued = queued
		s.paused = previousPaused
		for _, item := range queued {
			s.ids[item.input.ID] = struct{}{}
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	for _, item := range queued {
		m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(item.input), InputID: item.input.ID, State: TurnInputCancelled, Intent: item.input.Intent})
	}
	return nil
}

// StopSession cancels execution and joins finalization before callers delete
// backend history. A late queue write must not recreate a deleted directory.
func (m *TurnManager) StopSession(ctx context.Context, sessionID string) error {
	if err := m.CancelAll(sessionID); err != nil {
		return err
	}
	s := m.session(sessionID)
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unpause lets a recovered queue continue after the next turn without
// starting it now.
func (m *TurnManager) Unpause(sessionID string) error {
	s := m.session(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.queueLoaded {
		return s.queueErr
	}
	if !s.paused {
		return nil
	}
	s.paused = false
	if err := m.persistLocked(sessionID, s); err != nil {
		s.paused = true
		return err
	}
	return nil
}

func (m *TurnManager) Resume(sessionID string) bool {
	if !m.beginWork() {
		return false
	}
	defer m.workers.Done()
	s := m.session(sessionID)
	s.mu.Lock()
	if !s.queueLoaded || m.ctx.Err() != nil {
		s.mu.Unlock()
		return false
	}
	previousPaused := s.paused
	s.paused = false
	i := runnable(s.queued)
	if s.active != nil || s.running || i < 0 {
		if err := m.persistLocked(sessionID, s); err != nil {
			s.paused = previousPaused
		}
		s.mu.Unlock()
		return false
	}
	next := s.queued[i]
	s.queued = slices.Delete(s.queued, i, i+1)
	s.active = next
	s.running = true
	s.cancelRequested = false
	if err := m.persistLocked(sessionID, s); err != nil {
		s.active = nil
		s.queued = slices.Insert(s.queued, i, next)
		s.running = false
		s.paused = true
		s.mu.Unlock()
		return false
	}
	s.done = make(chan struct{})
	s.mu.Unlock()
	m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(next.input), InputID: next.input.ID, State: TurnInputActive, Intent: next.input.Intent})
	m.startSession(sessionID, s)
	return true
}

func (m *TurnManager) Snapshot(sessionID string) TurnSnapshot {
	s := m.session(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := TurnSnapshot{Paused: s.paused, Compacting: s.compacting || s.active != nil && s.active.input.Compact, Features: m.Features(sessionID), Error: s.queueErr}
	if s.active != nil {
		out.Inputs = append(out.Inputs, TurnInputSnapshot{
			Input: CloneTurnInput(s.active.input), ID: s.active.input.ID, State: TurnInputActive, Intent: s.active.input.Intent,
		})
	}
	for _, item := range s.steered {
		out.Inputs = append(out.Inputs, TurnInputSnapshot{
			Input: CloneTurnInput(item.input), ID: item.input.ID, State: TurnInputSteered, Intent: item.input.Intent,
		})
	}
	for i, item := range s.queued {
		out.Inputs = append(out.Inputs, TurnInputSnapshot{
			Input: CloneTurnInput(item.input), ID: item.input.ID, State: TurnInputQueued, Intent: item.input.Intent, Position: i + 1,
		})
	}
	return out
}

func (m *TurnManager) emitQueuePositions(sessionID string, s *managedTurnSession) {
	s.mu.Lock()
	items := make([]*managedTurnInput, 0, len(s.queued))
	for _, item := range s.queued {
		items = append(items, &managedTurnInput{input: CloneTurnInput(item.input)})
	}
	s.mu.Unlock()
	for i, item := range items {
		m.emit(TurnEvent{SessionID: sessionID, Input: CloneTurnInput(item.input), InputID: item.input.ID, State: TurnInputQueued, Intent: item.input.Intent, Position: i + 1})
	}
}

func (m *TurnManager) releaseID(s *managedTurnSession, inputID string) {
	s.mu.Lock()
	delete(s.ids, inputID)
	s.mu.Unlock()
}

func (m *TurnManager) Close() {
	m.cancel()
	m.mu.Lock()
	sessions := make(map[string]*managedTurnSession, len(m.sessions))
	maps.Copy(sessions, m.sessions)
	m.mu.Unlock()
	for id, s := range sessions {
		s.mu.Lock()
		cancel := s.cancel
		s.mu.Unlock()
		if cancel != nil {
			cancel(nil)
		}
		m.agent.Cancel(id)
	}
	m.workers.Wait()
}
