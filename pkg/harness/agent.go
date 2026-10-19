package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

var errYieldStopped = errors.New("yield stopped")

// ErrTurnInProgress means Send was called while another turn was active.
var ErrTurnInProgress = errors.New("agent turn already in progress")

// ErrEmptyInput means Send was called without any content.
var ErrEmptyInput = errors.New("agent input is empty")

const maxStreamRetries = 5

type Agent struct {
	*Config

	// PrepareTurn runs with the operation reserved, before hooks and tool
	// capture. Set it before using the agent. It belongs to this instance;
	// delegated agents inherit Config, not their parent's preparation.
	PrepareTurn func(context.Context, []Content) ([]Content, error)

	// Resources belongs to this instance. Close releases it after workers exit
	// and before SessionEnd hooks. Set it before using the agent.
	Resources io.Closer

	// Events is the canonical append-only runtime ledger. Messages is its
	// materialized conversation projection and remains exported for source
	// compatibility; provider context is maintained separately.
	Events   []RuntimeEvent
	Messages []Message
	Usage    Usage
	Revision uint64

	// Recorder is instance-scoped on purpose: derived subagent configs must not
	// accidentally append child events to their parent's journal.
	Recorder EventRecorder

	stateMu         sync.RWMutex
	contextMessages []Message
	contextSet      bool
	ContextRevision uint64
	contextUsage    contextUsageAnchor
	runtimeIndex    runtimeEventIndex
	runtimeIndexSet bool

	queueMu       sync.Mutex
	running       bool
	finishing     bool
	closed        bool
	cancel        context.CancelCauseFunc
	operationDone chan struct{}
	turnID        string
	turnTools     []tool.Tool
	toolsPinned   bool
	pendingInput  []Message
	started       bool // Only accessed by the operation owner.

	backgroundCtx    context.Context
	cancelBackground context.CancelFunc
	background       sync.WaitGroup

	toolRunsMu sync.Mutex
	toolRuns   map[string]*toolRun
}

type toolRun struct {
	signature string
	done      chan struct{}
	result    tool.Result // Published by closing done; immutable afterward.
}

// Running reports whether a turn is currently active.
func (a *Agent) Running() bool {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	return a.running
}

// QueueInput adds guidance to the active run. The agent consumes queued input
// at the next safe model boundary. It returns false when no run is active so
// callers can preserve the input as a normal follow-up instead.
func (a *Agent) QueueInput(input []Content) bool { return a.QueueInputWithID(input, "") }

func (a *Agent) QueueInputWithID(input []Content, id string) bool {
	if len(input) == 0 {
		return false
	}
	input = CloneContent(input)
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if !a.running || a.finishing || a.closed {
		return false
	}
	message := userMessage(input)
	message.InputID = id
	a.pendingInput = append(a.pendingInput, message)
	return true
}

// Send starts exactly one turn. It never queues implicitly: callers that want
// to guide the active turn must use QueueInput, while FIFO follow-ups belong in
// a caller-owned session orchestrator. Setup errors are returned immediately;
// failures after the turn starts are yielded by the returned stream. Consume
// the stream exactly once, including after cancellation, to settle the turn.
func (a *Agent) Send(ctx context.Context, input []Content) (iter.Seq2[Message, error], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, ErrEmptyInput
	}
	input = CloneContent(input)

	ctx, endOperation, err := a.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancelBudget := a.Config.withTaskBudget(ctx)
	release := func() {
		cancelBudget()
		endOperation()
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	stream, err := a.send(ctx, input)
	if err != nil {
		return nil, err
	}
	// Setup has finished using session resources. The caller now owns stream
	// consumption; Close can cancel it without waiting on an idle consumer.
	a.detachStream()
	handedOff = true
	var consumed atomic.Bool
	return func(yield func(Message, error) bool) {
		if !consumed.CompareAndSwap(false, true) {
			yield(Message{}, errors.New("agent turn stream already consumed"))
			return
		}
		defer release()
		stream(yield)
	}, nil
}

func (a *Agent) send(ctx context.Context, input []Content) (iter.Seq2[Message, error], error) {
	if a.PrepareTurn != nil {
		var err error
		input, err = a.PrepareTurn(ctx, input)
		if err != nil {
			return nil, err
		}
		input = CloneContent(input)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, ErrEmptyInput
	}
	a.pinTools()
	runtime := a.hookRuntime(ctx)
	runtime.TurnID = uuid.NewString()
	ctx = hook.WithRuntime(ctx, runtime)
	ctx, telemetryInvocation := a.Telemetry.StartAgent(ctx, telemetry.AgentRequest{
		ConversationID: conversationID(ctx, a.CacheKey),
	})
	turnUsageBefore := a.UsageSnapshot()
	if err := a.recordEvents(RuntimeEvent{
		Type: EventTurnStarted, TurnID: runtime.TurnID, Model: runtime.Model,
	}); err != nil {
		telemetryInvocation.End(telemetry.Outcome{Err: err})
		return nil, err
	}
	a.queueMu.Lock()
	a.turnID = runtime.TurnID
	a.finishing = false
	a.queueMu.Unlock()

	// Setup and streaming share one terminal path. A panic in a startup hook
	// must settle the ledger just like a panic after stream consumption starts.
	finished := false
	var terminalErr error
	finish := func(status RuntimeStatus, outcomeErr error) error {
		if finished {
			return terminalErr
		}
		finished = true
		defer func() {
			telemetryInvocation.End(telemetry.Outcome{Err: errors.Join(outcomeErr, terminalErr)})
		}()
		terminalErr = a.finishTurn(ctx, runtime.TurnID, status, outcomeErr, turnUsageBefore)
		return terminalErr
	}
	defer func() {
		if r := recover(); r != nil {
			finish(RuntimeFailed, fmt.Errorf("agent turn setup panicked: %v", r))
			panic(r)
		}
	}()

	failSetup := func(err error) (iter.Seq2[Message, error], error) {
		status := RuntimeFailed
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = RuntimeInterrupted
		}
		return nil, errors.Join(err, finish(status, err))
	}

	if err := ctx.Err(); err != nil {
		return failSetup(err)
	}
	var sessionStartOutcome hook.Outcome
	var sessionStartErr error
	if !a.started {
		a.started = true
		source := runtime.StartSource
		if source == "" {
			source = "startup"
		}
		sessionStartOutcome, sessionStartErr = a.runSessionStartHooks(ctx, source)
	}
	if sessionStartErr != nil {
		return failSetup(sessionStartErr)
	}
	if err := ctx.Err(); err != nil {
		return failSetup(err)
	}
	if sessionStartOutcome.Stop {
		if sessionStartOutcome.Reason == "" {
			sessionStartOutcome.Reason = "session stopped by hook"
		}
		return failSetup(errors.New(sessionStartOutcome.Reason))
	}

	message := userMessage(input)
	message.InputID = InputIDFromContext(ctx)
	if err := a.appendInputs(ctx, message); err != nil {
		return failSetup(err)
	}

	maxTurns := a.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}

	return func(yield func(Message, error) bool) {
		status := RuntimeCompleted
		var outcomeErr error
		consumerOpen := true
		defer func() {
			r := recover()
			if r != nil {
				status = RuntimeFailed
				outcomeErr = fmt.Errorf("agent turn panicked: %v", r)
			}
			finishErr := finish(status, outcomeErr)
			if r != nil {
				panic(r)
			}
			if finishErr != nil && consumerOpen {
				yield(Message{}, finishErr)
			}
		}()

		stop := func(err error) {
			err = taskOutcomeError(ctx, err)
			outcomeErr = err
			_, hookStopped := errors.AsType[hookStopError](err)
			if hookStopped || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errYieldStopped) {
				status = RuntimeInterrupted
			} else if errors.Is(err, ErrTurnIncomplete) {
				status = RuntimeIncomplete
			} else {
				status = RuntimeFailed
			}
			if errors.Is(err, errYieldStopped) {
				consumerOpen = false
				return
			}
			if hookStopped {
				return
			}
			if !yield(Message{}, err) {
				consumerOpen = false
			}
		}

		turns := 0
		var completion completionState
		nativeContinuation := false
		stopHookActive := false
		for {
			if err := ctx.Err(); err != nil {
				stop(err)
				return
			}
			// Give the original prompt its own first request. Later requests
			// consume steering, including input received during Stop hooks.
			if turns > 0 {
				pending := a.takePendingInput()
				if len(pending) > 0 {
					completion = completionState{}
					nativeContinuation = false
				}
				if err := a.appendInputs(ctx, pending...); err != nil {
					stop(err)
					return
				}
			}

			if err := a.repairToolHistory(nativeContinuation); err != nil {
				stop(err)
				return
			}
			if err := a.syncContextInstructions(); err != nil {
				stop(err)
				return
			}

			stepCtx, req, err := a.buildStepRequest(ctx, nativeContinuation)
			if err != nil {
				stop(err)
				return
			}

			// If a previous round already completed a final answer and only
			// continued to confirm finish_turn, switching mid-turn to a model
			// that does not use finish_turn completes the turn without asking
			// the new model to regenerate the answer.
			if !req.requireFinish && completion.awaitingFinish {
				done, err := a.finishStep(ctx, stepCtx, completion.answer, false, false, nil, &completion, &stopHookActive)
				if done {
					if err != nil {
						stop(err)
					}
					return
				}
				continue
			}

			resp, err := a.completeWithRetry(stepCtx, runtime.TurnID, req, yield)
			if err != nil {
				stop(err)
				return
			}
			turns++
			nativeContinuation = resp.endTurn != nil && !*resp.endTurn

			EmitStreamEvent(ctx, StreamEventCommit)
			if err := ctx.Err(); err != nil {
				stop(err)
				return
			}

			// A filtered terminal response cannot dispatch further tools or
			// automatically continue. Earlier completed items may have run.
			needsFollowUp := false
			var completionErr error
			filtered := resp.incomplete && resp.incompleteReason == "content_filter"
			if !filtered {
				calls, finished, finishRejected, err := a.resolveFinishCalls(req, resp, extractToolCalls(resp.messages), completion.answer, yield)
				if err != nil {
					stop(err)
					return
				}
				if err := a.processToolCalls(stepCtx, calls[resp.dispatched:], req.tools, yield); err != nil {
					stop(err)
					return
				}
				var notice *Message
				needsFollowUp, notice, completionErr = completion.advance(resp, req.requireFinish, finished, finishRejected, len(calls) > 0)
				if notice != nil {
					if err := a.appendMessages(*notice); err != nil {
						stop(err)
						return
					}
				}
			}

			last := lastAssistantText(resp.messages)
			if req.requireFinish && last == "" {
				last = completion.answer
			}
			done, err := a.finishStep(ctx, stepCtx, last, needsFollowUp, resp.incomplete, completionErr, &completion, &stopHookActive)
			if done {
				if err != nil {
					stop(err)
				}
				return
			}
			if maxTurns > 0 && turns >= maxTurns {
				stop(ErrMaxTurnsExceeded)
				return
			}
		}
	}, nil
}

func (a *Agent) runSessionStartHooks(ctx context.Context, source string) (hook.Outcome, error) {
	var combined hook.Outcome
	var parts []string
	for i, h := range a.Hooks.SessionStart {
		if err := ctx.Err(); err != nil {
			return combined, err
		}
		outcome, err := h(ctx, source)
		if err != nil {
			hook.ReportError(ctx, "SessionStart", "", i, err)
			continue
		}
		parts = append(parts, outcome.AdditionalContext...)
		if outcome.Stop && !combined.Stop {
			combined = outcome
		}
	}
	if err := ctx.Err(); err != nil {
		return combined, err
	}
	if len(parts) > 0 {
		if err := a.appendMessages(hiddenContextMessage(strings.Join(parts, "\n\n"))); err != nil {
			return combined, err
		}
	}
	return combined, nil
}

func (a *Agent) runStopHooks(ctx context.Context, lastAssistantMessage string, active bool) hook.Outcome {
	var combined hook.Outcome
	for i, h := range a.Hooks.Stop {
		outcome, err := h(ctx, lastAssistantMessage, active)
		if err != nil {
			hook.ReportError(ctx, "Stop", "", i, err)
			continue
		}
		if outcome.Stop && !combined.Stop {
			combined.Stop = true
			combined.Reason = outcome.Reason
		}
		if outcome.Block && !combined.Block {
			combined.Block = true
			combined.Reason = outcome.Reason
		}
	}
	// Child completion uses the same request context and continuation limits
	// as ordinary Stop hooks. Do not finish the child while a Stop hook blocks.
	runtime := hook.RuntimeFromContext(ctx)
	if runtime.AgentID == "" || combined.Stop || combined.Block {
		return combined
	}
	for i, h := range a.Hooks.SubagentStop {
		outcome, err := h(ctx, runtime.AgentID, runtime.AgentType, lastAssistantMessage, active)
		if err != nil {
			hook.ReportError(ctx, "SubagentStop", "", i, err)
			continue
		}
		if outcome.Stop {
			return outcome
		}
		if outcome.Block && !combined.Block {
			combined = outcome
		}
	}
	return combined
}

func contentText(input []Content) string {
	var parts []string
	for _, c := range input {
		if text := c.AsText(); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func lastAssistantText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant {
			if text := contentText(messages[i].Content); text != "" {
				return text
			}
		}
	}
	return ""
}

func endsWithNonterminalText(messages []Message) bool {
	nonterminal := false
	for _, message := range messages {
		if message.Role != RoleAssistant {
			continue
		}
		for _, content := range message.Content {
			if content.Refusal != "" {
				return false
			}
			if content.Text != "" {
				nonterminal = message.Phase == PhaseCommentary || message.Phase == PhasePartialAnswer
			}
		}
	}
	return nonterminal
}

func hiddenContextMessage(text string) Message {
	return Message{
		Role:    RoleUser,
		Hidden:  true,
		Content: []Content{{Text: text}},
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// completeRun gives every provider invocation its own durable lifecycle. A
// retry is therefore a new Run rather than an invisible overwrite of the
// failed attempt. Usage is applied only by the terminal event.
func (a *Agent) completeRun(ctx, inferenceCtx context.Context, turnID string, req *request, yield func(Message, error) bool) (resp *response, runErr error) {
	runID := uuid.NewString()
	if err := a.recordEvents(RuntimeEvent{
		Type: EventRunStarted, TurnID: turnID, RunID: runID, Model: req.model,
	}); err != nil {
		return nil, err
	}

	toolCtx, cancelTools := withToolStop(ctx)
	defer cancelTools()
	inferenceCtx, cancelInference := context.WithCancel(inferenceCtx)
	defer cancelInference()
	stopInference := context.AfterFunc(toolCtx, func() {
		if _, stopped := errors.AsType[hookStopError](context.Cause(toolCtx)); stopped {
			cancelInference()
		}
	})
	defer stopInference()
	round := streamToolRound{agent: a, ctx: toolCtx, tools: req.tools}
	var committed []Message
	consumerOpen := true
	emit := func(message Message, err error) bool {
		if consumerOpen {
			consumerOpen = yield(message, err)
		}
		if !consumerOpen {
			cancelTools()
		}
		return consumerOpen
	}
	defer func() {
		if p := recover(); p != nil {
			cancelTools()
			_ = round.drain(func(Message, error) bool { return false })
			panic(p)
		}
		drainErr := round.drain(emit)
		if _, stopped := errors.AsType[hookStopError](drainErr); stopped && runErr == context.Canceled {
			// This stream was canceled by the hook, not by the provider.
			runErr = nil
		}
		runErr = errors.Join(runErr, drainErr)
		if runErr != nil {
			// A finish marker is only valid after the response completes. Give
			// retained markers a rejected result before retrying a failed stream.
			_, _, _, finishErr := a.resolveFinishCalls(req, &response{messages: committed, incomplete: true}, extractToolCalls(committed), "", emit)
			runErr = errors.Join(runErr, finishErr)
		}
		if resp != nil {
			resp.dispatched = len(round.calls)
		}
	}()
	streamReq := *req
	streamReq.onOutputItem = func(messages []Message) error {
		if err := a.appendMessages(messages...); err != nil {
			return err
		}
		committed = append(committed, messages...)
		EmitStreamEvent(ctx, StreamEventCommit)
		for _, call := range extractToolCalls(messages) {
			if req.handlesFinishTool(call.Name) {
				continue
			}
			open := emit(toolCallMessage(call), nil)
			round.start(call)
			if !open {
				return errYieldStopped
			}
		}
		return nil
	}
	resp, runErr = a.complete(inferenceCtx, &streamReq, emit)
	if _, stopped := errors.AsType[hookStopError](context.Cause(toolCtx)); stopped && inferenceCtx.Err() != nil {
		// Cancellation can close the transport as EOF before its context error
		// is delivered. Normalize only the inference error, retaining any later
		// local persistence failures when the round is drained.
		_, streamFailed := errors.AsType[*streamFailure](runErr)
		if streamFailed || errors.Is(runErr, context.Canceled) {
			runErr = context.Canceled
		}
	}
	if runErr == nil && resp != nil && (len(resp.messages) < len(committed) || !messagesEqual(committed, resp.messages[:len(committed)])) {
		runErr = fmt.Errorf("final response does not match its committed output items")
	}
	if resp != nil {
		chargeTaskUsage(ctx, resp.usage)
	}
	status := RuntimeCompleted
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, errYieldStopped) {
			status = RuntimeInterrupted
		} else {
			status = RuntimeFailed
		}
	}
	terminal := &RuntimeTerminal{Status: status}
	if runErr != nil {
		terminal.Error = runErr.Error()
	}
	event := RuntimeEvent{
		Type: EventRunTerminal, TurnID: turnID, RunID: runID, Model: req.model, Terminal: terminal,
	}
	if resp != nil {
		usage := resp.usage
		if usage.InputTokens > 0 {
			usage.LastInputTokens = usage.InputTokens
		}
		event.Usage = &usage
	}
	events := make([]RuntimeEvent, 0, 1)
	if runErr == nil && resp != nil {
		events = append(events, messageEvents(resp.messages[len(committed):]...)...)
	}
	events = append(events, event)
	if err := a.recordEvents(events...); err != nil {
		if runErr != nil {
			return nil, errors.Join(runErr, fmt.Errorf("record terminal fact: %w", err))
		}
		return nil, err
	}
	if runErr == nil && resp != nil {
		committed = resp.messages
		a.anchorContextUsage(req, resp)
	}
	return resp, runErr
}

func (a *Agent) finishTurn(ctx context.Context, turnID string, status RuntimeStatus, outcomeErr error, before Usage) error {
	a.queueMu.Lock()
	a.finishing = true
	queuedMessages := a.pendingInput
	a.pendingInput = nil
	a.queueMu.Unlock()

	appendErr := a.appendInputs(ctx, queuedMessages...)
	if appendErr != nil && outcomeErr == nil {
		status = RuntimeFailed
		outcomeErr = appendErr
	}
	after := a.UsageSnapshot()
	delta := Usage{
		InputTokens:              after.InputTokens - before.InputTokens,
		OutputTokens:             after.OutputTokens - before.OutputTokens,
		ReasoningTokens:          after.ReasoningTokens - before.ReasoningTokens,
		CacheReadInputTokens:     after.CacheReadInputTokens - before.CacheReadInputTokens,
		CacheCreationInputTokens: after.CacheCreationInputTokens - before.CacheCreationInputTokens,
	}
	terminal := &RuntimeTerminal{Status: status}
	if outcomeErr != nil {
		terminal.Error = outcomeErr.Error()
	}
	recordErr := a.recordEvents(RuntimeEvent{
		Type: EventTurnTerminal, TurnID: turnID, Usage: &delta, Terminal: terminal,
	})
	if status == RuntimeInterrupted && errors.Is(context.Cause(ctx), ErrInterrupted) {
		a.runInterruptHooks(ctx)
	}
	return errors.Join(appendErr, recordErr)
}

func (a *Agent) runInterruptHooks(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	for _, h := range a.Hooks.Interrupt {
		h(ctx)
	}
}

func extractToolCalls(messages []Message) []ToolCall {
	var calls []ToolCall

	for _, m := range messages {
		for _, c := range m.Content {
			if c.ToolCall != nil {
				calls = append(calls, *c.ToolCall)
			}
		}
	}

	return calls
}

func (a *Agent) processToolCalls(ctx context.Context, calls []ToolCall, tools []tool.Tool, yield func(Message, error) bool) error {
	ctx, cancel := withToolStop(ctx)
	defer cancel()
	a.beginToolRound()

	for start := 0; start < len(calls); {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		end := start + 1

		if a.isReadOnly(calls[start], tools) {
			for end < len(calls) && a.isReadOnly(calls[end], tools) {
				end++
			}
		}

		var err error

		if end-start > 1 {
			err = a.processToolCallsParallel(ctx, calls[start:end], tools, yield)
		} else {
			err = a.processToolCall(ctx, calls[start], tools, yield)
		}

		if err != nil {
			return err
		}

		start = end
	}

	return context.Cause(ctx)
}

func (a *Agent) processToolCall(ctx context.Context, tc ToolCall, tools []tool.Tool, yield func(Message, error) bool) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if !yield(toolCallMessage(tc), nil) {
		return errYieldStopped
	}

	outcome := a.runSingleToolCallOutcome(ctx, tc, tools)
	resultMsg := toolResultMessage(tc, outcome.result)
	if !outcome.recorded {
		if err := a.appendMessages(resultMsg); err != nil {
			return err
		}
	}

	if !yield(resultMsg, nil) {
		return errYieldStopped
	}

	return context.Cause(ctx)
}

func (a *Agent) processToolCallsParallel(ctx context.Context, calls []ToolCall, tools []tool.Tool, yield func(Message, error) bool) error {
	for _, tc := range calls {
		if !yield(toolCallMessage(tc), nil) {
			return errYieldStopped
		}
	}

	type completion struct {
		index   int
		outcome toolCallOutcome
	}

	outcomes := make([]toolCallOutcome, len(calls))
	completionOrder := make([]int, 0, len(calls))
	ch := make(chan completion, len(calls))
	jobs := make(chan int, len(calls))

	parallelism := a.MaxParallelTools
	if parallelism == 0 {
		parallelism = DefaultMaxParallelTools
	}
	if parallelism < 0 || parallelism > len(calls) {
		parallelism = len(calls)
	}

	for range parallelism {
		go func() {
			for i := range jobs {
				ch <- completion{i, a.runSingleToolCallOutcomeDeferred(ctx, calls[i], tools)}
			}
		}()
	}
	for i := range calls {
		jobs <- i
	}
	close(jobs)

	for range calls {
		c := <-ch
		outcomes[c.index] = c.outcome
		completionOrder = append(completionOrder, c.index)
	}

	resultMessages := make([]Message, len(calls))
	var batchEvents []RuntimeEvent
	allDeferred := len(calls) > 0
	for i, tc := range calls {
		commit := outcomes[i].deferredCommit
		if commit == nil {
			allDeferred = false
			break
		}
		batchEvents = append(batchEvents, toolOutcomeEvents(tc, outcomes[i].result, *commit)...)
		resultMessages[i] = toolResultMessage(tc, outcomes[i].result)
	}
	if !allDeferred || a.recordEvents(batchEvents...) != nil {
		var unrecorded []Message
		for i, tc := range calls {
			outcome := a.commitToolOutcome(tc, outcomes[i])
			resultMessages[i] = toolResultMessage(tc, outcome.result)
			if !outcome.recorded {
				unrecorded = append(unrecorded, resultMessages[i])
			}
		}
		if err := a.appendMessages(unrecorded...); err != nil {
			return err
		}
	}
	for _, i := range completionOrder {
		if !yield(resultMessages[i], nil) {
			return errYieldStopped
		}
	}

	return context.Cause(ctx)
}

type toolCallOutcome struct {
	result         tool.Result
	recorded       bool
	deferredCommit *toolCommit
}

type toolCommit struct {
	operationID string
	turnID      string
	effect      tool.Effect
}

func toolCallMessage(tc ToolCall) Message {
	presentation := NewToolPresentation(tc.Name, tc.Kind, tc.Args, tc.Locations)
	return Message{
		Role: RoleAssistant,
		Content: []Content{{ToolCall: &ToolCall{
			ID: tc.ID, Name: tc.Name, Kind: tc.Kind, Args: tc.Args,
			Locations: tc.Locations, Presentation: presentation,
		}}},
	}
}

const imageResultPlaceholder = "[image attached below]"

const (
	interruptedToolResult = "error: interrupted — the request was canceled before this tool call finished"
	interruptedWaitResult = "error: interrupted while waiting for the original tool call to finish"
)

func toolResultMessage(tc ToolCall, result tool.Result) Message {
	message := Message{
		Role: RoleAssistant,
		Content: []Content{{ToolResult: &ToolResult{
			ID: tc.ID, Name: tc.Name, Kind: tc.Kind, Args: tc.Args,
			Locations: tc.Locations, Presentation: NewToolPresentation(tc.Name, tc.Kind, tc.Args, tc.Locations),
			Content: result.Content, IsError: result.IsError, Metadata: result.Metadata,
		}}},
	}
	if tool.IsImageResult(result.Content) {
		message.Content[0].ToolResult.Content = imageResultPlaceholder
		message.Content = append(message.Content, Content{File: &File{Data: result.Content}})
	}
	return message
}

func (a *Agent) runSingleToolCall(ctx context.Context, tc ToolCall, tools []tool.Tool) tool.Result {
	return a.runSingleToolCallOutcome(ctx, tc, tools).result
}

func (a *Agent) runSingleToolCallOutcome(ctx context.Context, tc ToolCall, tools []tool.Tool) toolCallOutcome {
	return a.runSingleToolCallOutcomeMode(ctx, tc, tools, false)
}

func (a *Agent) runSingleToolCallOutcomeDeferred(ctx context.Context, tc ToolCall, tools []tool.Tool) toolCallOutcome {
	return a.runSingleToolCallOutcomeMode(ctx, tc, tools, true)
}

func (a *Agent) runSingleToolCallOutcomeMode(ctx context.Context, tc ToolCall, tools []tool.Tool, deferCommit bool) (outcome toolCallOutcome) {
	var run *toolRun
	defer func() {
		if r := recover(); r != nil {
			outcome = toolCallOutcome{result: tool.Error(fmt.Sprintf("error: tool %s panicked: %v", tc.Name, r))}
		}
		// Cache failures too, so duplicate callers receive the same outcome.
		if run != nil {
			run.result = cloneToolResult(outcome.result)
			close(run.done)
		}
	}()
	if tc.ID == "" {
		return a.executeToolOperation(ctx, tc, tools, deferCommit)
	}
	if result, ok := a.retainedToolResult(tc); ok {
		return toolCallOutcome{result: result}
	}

	signature := tc.Name + "\x00" + tc.Args
	a.toolRunsMu.Lock()
	if a.toolRuns == nil {
		a.toolRuns = make(map[string]*toolRun)
	}
	if existing := a.toolRuns[tc.ID]; existing != nil {
		if existing.signature != signature {
			a.toolRunsMu.Unlock()
			return toolCallOutcome{result: tool.Error(fmt.Sprintf("error: tool call ID %s was reused with different arguments", tc.ID))}
		}
		done := existing.done
		a.toolRunsMu.Unlock()
		select {
		case <-done:
			return toolCallOutcome{result: cloneToolResult(existing.result)}
		case <-ctx.Done():
			return toolCallOutcome{result: tool.Error(interruptedWaitResult)}
		}
	}
	run = &toolRun{signature: signature, done: make(chan struct{})}
	a.toolRuns[tc.ID] = run
	a.toolRunsMu.Unlock()
	return a.executeToolOperation(ctx, tc, tools, deferCommit)
}

func (a *Agent) executeToolOperation(ctx context.Context, tc ToolCall, tools []tool.Tool, deferCommit bool) toolCallOutcome {
	operationID := uuid.NewString()
	effect := a.toolEffect(tc, tools)
	t := findTool(tc.Name, tools)
	description := ""
	toolType := "function"
	mcpMethod := ""
	mcpProtocolVersion := ""
	if t != nil {
		description = t.Description
		if t.Telemetry.ToolType != "" {
			toolType = t.Telemetry.ToolType
		}
		mcpMethod = t.Telemetry.MCPMethod
		mcpProtocolVersion = t.Telemetry.MCPProtocolVersion
	}
	ctx, telemetryExecution := a.Telemetry.StartTool(ctx, telemetry.ToolRequest{
		Name:               tc.Name,
		Description:        description,
		CallID:             tc.ID,
		Type:               toolType,
		MCPMethod:          mcpMethod,
		MCPProtocolVersion: mcpProtocolVersion,
		Arguments:          genericToolTelemetryValue(mcpMethod, tc.Args),
	})
	telemetryEnded := false
	defer func() {
		if !telemetryEnded {
			telemetryExecution.End(telemetry.Outcome{ErrorType: "_OTHER"})
		}
	}()
	started := RuntimeEvent{
		Type:        EventToolStarted,
		TurnID:      hook.RuntimeFromContext(ctx).TurnID,
		OperationID: operationID,
		Tool:        &RuntimeTool{CallID: tc.ID, Name: tc.Name, Args: tc.Args, Effect: string(effect)},
	}
	if err := a.recordEvents(started); err != nil {
		telemetryExecution.End(telemetry.Outcome{Err: err})
		telemetryEnded = true
		return toolCallOutcome{result: tool.Error(fmt.Sprintf("error: could not durably start tool call: %v", err))}
	}

	result := a.executeSingleToolCall(ctx, tc, tools)
	toolOutcome := telemetry.Outcome{}
	if result.IsError {
		toolOutcome.ErrorType = "_OTHER"
		if result.Content == interruptedToolResult || result.Content == interruptedWaitResult {
			toolOutcome.ErrorType = "canceled"
		} else if strings.Contains(result.Content, "time limit") || strings.Contains(result.Content, "deadline") {
			toolOutcome.ErrorType = "timeout"
		}
	}
	if mcpMethod == "" {
		telemetryExecution.SetResult(genericToolResultTelemetryValue(result.Content))
	}
	telemetryExecution.End(toolOutcome)
	telemetryEnded = true
	outcome := toolCallOutcome{
		result: result,
		deferredCommit: &toolCommit{
			operationID: operationID,
			turnID:      started.TurnID,
			effect:      effect,
		},
	}
	if deferCommit {
		return outcome
	}
	return a.commitToolOutcome(tc, outcome)
}

func (a *Agent) commitToolOutcome(tc ToolCall, outcome toolCallOutcome) toolCallOutcome {
	if outcome.deferredCommit == nil {
		return outcome
	}
	if err := a.recordEvents(toolOutcomeEvents(tc, outcome.result, *outcome.deferredCommit)...); err != nil {
		return toolCallOutcome{result: tool.Error(fmt.Sprintf("error: tool finished but its result and terminal fact could not be persisted: %v", err))}
	}
	return toolCallOutcome{result: outcome.result, recorded: true}
}

func toolOutcomeEvents(tc ToolCall, result tool.Result, commit toolCommit) []RuntimeEvent {
	status := RuntimeCompleted
	if result.IsError {
		status = RuntimeFailed
	}
	if result.Content == interruptedToolResult || result.Content == interruptedWaitResult {
		status = RuntimeInterrupted
	}
	terminal := &RuntimeTerminal{Status: status}
	if result.IsError {
		terminal.Error = result.Content
	}
	resultMessage := toolResultMessage(tc, result)
	return append(messageEvents(resultMessage), RuntimeEvent{
		Type:        EventToolTerminal,
		TurnID:      commit.turnID,
		OperationID: commit.operationID,
		Tool:        &RuntimeTool{CallID: tc.ID, Name: tc.Name, Args: tc.Args, Effect: string(commit.effect), IsError: result.IsError},
		Terminal:    terminal,
	})
}

func genericToolTelemetryValue(mcpMethod, arguments string) any {
	if mcpMethod != "" || !json.Valid([]byte(arguments)) {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil
	}
	return object
}

func genericToolResultTelemetryValue(content string) map[string]any {
	if json.Valid([]byte(content)) {
		decoder := json.NewDecoder(strings.NewReader(content))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err == nil {
			if object, ok := decoded.(map[string]any); ok {
				return object
			}
			return map[string]any{"content": decoded}
		}
	}
	return map[string]any{"content": content}
}

func (a *Agent) beginToolRound() {
	a.toolRunsMu.Lock()
	a.toolRuns = nil
	a.toolRunsMu.Unlock()
}

func (a *Agent) retainedToolResult(tc ToolCall) (tool.Result, bool) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	for i := len(a.Messages) - 1; i >= 0; i-- {
		for j := len(a.Messages[i].Content) - 1; j >= 0; j-- {
			result := a.Messages[i].Content[j].ToolResult
			if result == nil || result.ID != tc.ID {
				continue
			}
			if result.Name != tc.Name || result.Args != tc.Args {
				return tool.Error(fmt.Sprintf("error: tool call ID %s was reused with different arguments", tc.ID)), true
			}
			if result.Content == interruptedToolResult || result.Content == interruptedWaitResult {
				return tool.Result{}, false
			}
			content := result.Content
			if content == imageResultPlaceholder && j+1 < len(a.Messages[i].Content) {
				if file := a.Messages[i].Content[j+1].File; file != nil {
					content = file.Data
				}
			}
			return tool.Result{Content: content, IsError: result.IsError, Metadata: maps.Clone(result.Metadata)}, true
		}
	}
	return tool.Result{}, false
}

func cloneToolResult(result tool.Result) tool.Result {
	result.Metadata = maps.Clone(result.Metadata)
	return result
}

func (a *Agent) executeSingleToolCall(ctx context.Context, tc ToolCall, tools []tool.Tool) (result tool.Result) {
	// Contain executor and hook panics inside the operation so its caller can
	// persist the result and terminal fact. Preserve metadata from any work
	// already completed before a post-tool hook panicked.
	defer func() {
		if r := recover(); r != nil {
			result.Content = fmt.Sprintf("error: tool %s panicked: %v", tc.Name, r)
			result.IsError = true
		}
	}()
	started := time.Now()
	t := findTool(tc.Name, tools)

	timeout := a.ToolTimeout
	if timeout == 0 && t != nil {
		timeout = t.Timeout
	}
	if timeout == 0 {
		timeout = DefaultToolTimeout
	}
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	if err := ctx.Err(); err != nil {
		return toolExecutionError(ctx, err, timeout, started)
	}

	ctx = tool.WithProgressCall(ctx, tc.ID)
	var usageMu sync.Mutex
	var usageErr error
	ctx = tool.WithUsageSink(ctx, func(d tool.UsageDelta) {
		usage := Usage{
			InputTokens:              d.InputTokens,
			OutputTokens:             d.OutputTokens,
			ReasoningTokens:          d.ReasoningTokens,
			CacheReadInputTokens:     d.CacheReadInputTokens,
			CacheCreationInputTokens: d.CacheCreationInputTokens,
		}
		if err := a.recordEvents(RuntimeEvent{Type: EventUsage, TurnID: hook.RuntimeFromContext(ctx).TurnID, Usage: &usage}); err != nil {
			usageMu.Lock()
			usageErr = errors.Join(usageErr, err)
			usageMu.Unlock()
		}
	})

	hc := tool.ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args}

	execute := true
	var hookContext []string

	for i, h := range a.Hooks.PreToolUse {
		outcome, err := h(ctx, hc)

		if err != nil {
			hook.ReportError(ctx, "PreToolUse", "", i, err)
			result = tool.Error(fmt.Sprintf("error: %v", err))
			execute = false
			break
		}
		hookContext = append(hookContext, outcome.AdditionalContext...)
		if len(outcome.UpdatedInput) > 0 && json.Valid(outcome.UpdatedInput) {
			hc.Args = string(outcome.UpdatedInput)
			tc.Args = hc.Args
		}
		if outcome.Block || outcome.Stop {
			reason := outcome.Reason
			if reason == "" {
				reason = "tool call blocked by hook"
			}
			result = tool.Error("error: " + reason)
			if outcome.Stop {
				stopToolTurn(ctx, reason)
			}
			execute = false
			break
		}
	}

	if execute {
		result = a.executeTool(hook.WithToolCall(ctx, hc), tc, t, timeout, started)
	}

	for i, h := range a.Hooks.PostToolUse {
		outcome, err := h(ctx, hc, result.Content)

		if err != nil {
			hook.ReportError(ctx, "PostToolUse", "", i, err)
			result.Content, result.IsError = fmt.Sprintf("error: %v", err), true
			break
		}
		if outcome.UpdatedResult != nil {
			result.Content = *outcome.UpdatedResult
		}
		hookContext = append(hookContext, outcome.AdditionalContext...)
		if outcome.Block || outcome.Stop {
			reason := outcome.Reason
			if reason == "" {
				reason = "agentic loop stopped by post-tool hook"
			}
			// Codex replaces the completed tool result with hook feedback. The
			// side effect has already happened, but the original output must not
			// continue into the next model request.
			result.Content = reason
			result.IsError = true
			if outcome.Stop {
				stopToolTurn(ctx, reason)
				break
			}
		}
	}
	if len(hookContext) > 0 && !tool.IsImageResult(result.Content) {
		result.Content += "\n\n<hook-context>\n" + strings.Join(hookContext, "\n\n") + "\n</hook-context>"
	}
	if !tool.IsImageResult(result.Content) {
		for i, transform := range a.Hooks.TransformToolResult {
			content, err := transform(ctx, hc, result.Content)
			if err != nil {
				hook.ReportError(ctx, "TransformToolResult", "", i, err)
				result.Content, result.IsError = fmt.Sprintf("error: %v", err), true
				break
			}
			result.Content = content
		}
		result.Content = boundToolResult(result.Content)
	}
	usageMu.Lock()
	deferredUsageErr := usageErr
	usageMu.Unlock()
	if deferredUsageErr != nil {
		result.Content, result.IsError = fmt.Sprintf("error: tool usage could not be persisted: %v", deferredUsageErr), true
	}

	return result
}

func (a *Agent) executeTool(ctx context.Context, tc ToolCall, t *tool.Tool, timeout time.Duration, started time.Time) tool.Result {
	if t == nil {
		return tool.Error(fmt.Sprintf("error: unknown tool %s", tc.Name))
	}
	if t.Execute == nil {
		return tool.Error(fmt.Sprintf("error: tool %s has no executor", tc.Name))
	}

	args := make(map[string]any)

	if tc.Args != "" {
		if err := json.Unmarshal([]byte(tc.Args), &args); err != nil {
			return tool.Error(fmt.Sprintf("error: failed to parse arguments: %v", err))
		}
	}

	// Hooks and streamed call notifications can cancel the request after
	// dispatch. Executors such as filesystem writes need this final check.
	var result tool.Result
	err := ctx.Err()
	if err == nil {
		result, err = t.Execute(ctx, args)
	}

	if err != nil {
		failure := toolExecutionError(ctx, err, timeout, started)
		failure.Metadata = result.Metadata
		return failure
	}

	return result
}

func toolExecutionError(ctx context.Context, err error, timeout time.Duration, started time.Time) tool.Result {
	// Rewrite only errors the context caused; a tool's own failure that
	// races a cancellation must stay visible as-is.
	switch {
	case errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled):
		return tool.Error(interruptedToolResult)
	case errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded):
		if timeout > 0 && time.Since(started) >= timeout {
			return tool.Error(fmt.Sprintf("error: tool call aborted after exceeding its %s time limit", timeout))
		}
		return tool.Error("error: tool call aborted — the request deadline expired before it finished")
	}
	return tool.Error(fmt.Sprintf("error: %v", err))
}

func (a *Agent) isReadOnly(tc ToolCall, tools []tool.Tool) bool {
	return a.toolEffect(tc, tools) == tool.EffectReadOnly
}

func (a *Agent) toolEffect(tc ToolCall, tools []tool.Tool) tool.Effect {
	t := findTool(tc.Name, tools)
	if t == nil || t.Effect == nil {
		return tool.EffectDynamic
	}

	var args map[string]any
	if tc.Args != "" {
		if err := json.Unmarshal([]byte(tc.Args), &args); err != nil {
			return tool.EffectDynamic
		}
	}

	return t.Effect(args)
}

func findTool(name string, tools []tool.Tool) *tool.Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}

	return nil
}

func userMessage(input []Content) Message {
	hidden := len(input) > 0
	for _, c := range input {
		if !c.Hidden {
			hidden = false
			break
		}
	}
	return Message{
		Role:    RoleUser,
		Hidden:  hidden,
		Content: input,
	}
}

func cutoffNotice(reason string) Message {
	if reason == "" {
		reason = "max_output_tokens"
	}

	text := fmt.Sprintf("<system-reminder>Your previous response was cut off before completing (%s); anything past the cutoff, including further tool calls, was dropped. Continue from where it stopped without repeating completed work.</system-reminder>", reason)

	return Message{
		Role:    RoleUser,
		Hidden:  true,
		Content: []Content{{Text: text}},
	}
}
