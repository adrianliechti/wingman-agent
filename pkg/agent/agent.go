package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

var errYieldStopped = errors.New("yield stopped")

type Agent struct {
	*Config

	Messages []Message
	Usage    Usage

	queueMu      sync.Mutex
	running      bool
	pendingInput [][]Content
}

// Send routes input one of two ways:
//   - if a turn loop is already running for this agent, the input is queued
//     and Send returns nil. The in-flight loop will drain the queue at its
//     next safe boundary (between iterations, never inside a tool_call /
//     tool_result pair).
//   - otherwise, the input opens a new turn and Send returns an iterator
//     over the stream. The loop's exit clears the running flag and discards
//     any queue leftovers, so a cancel that aborts the loop also drops
//     queued work.
func (a *Agent) Send(ctx context.Context, input []Content) iter.Seq2[Message, error] {
	a.queueMu.Lock()
	if a.running {
		if len(input) > 0 {
			a.pendingInput = append(a.pendingInput, input)
		}
		a.queueMu.Unlock()
		return nil
	}
	a.running = true
	a.queueMu.Unlock()

	a.Messages = append(a.Messages, userMessage(input))

	maxTurns := a.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}

	return func(yield func(Message, error) bool) {
		// Recover from panics so the running flag is always released —
		// otherwise the agent would refuse all future Send calls.
		defer func() {
			if r := recover(); r != nil {
				a.endRun()
				panic(r)
			}
		}()

		turns := 0
		for {
			turns++
			if maxTurns > 0 && turns > maxTurns {
				yield(Message{}, ErrMaxTurnsExceeded)
				a.endRun()
				return
			}

			a.removeOrphanedToolMessages()

			model := ""
			if a.Config.Model != nil {
				model = a.Model()
			}

			effort := ""
			if a.Config.Effort != nil {
				effort = a.Effort()
			}

			instructions := ""
			if a.Instructions != nil {
				instructions = a.Instructions()
			}

			var tools []tool.Tool
			if a.Tools != nil {
				tools = a.Tools()
			}

			req := &request{
				model:        model,
				effort:       effort,
				instructions: instructions,
				messages:     a.Messages,
				tools:        tools,
			}

			resp, err := complete(ctx, a.client, req, yield)

			if err != nil {
				if !errors.Is(err, errYieldStopped) && !errors.Is(err, context.Canceled) && isRecoverableError(err) {
					a.compactMessages(ctx)

					req.messages = a.Messages
					resp, err = complete(ctx, a.client, req, yield)
				}

				if err != nil {
					a.endRun()
					if err != errYieldStopped {
						yield(Message{}, err)
					}
					return
				}
			}

			a.Usage.InputTokens += resp.usage.InputTokens
			a.Usage.CachedTokens += resp.usage.CachedTokens
			a.Usage.OutputTokens += resp.usage.OutputTokens
			a.Messages = append(a.Messages, resp.messages...)

			calls := extractToolCalls(resp.messages)

			if len(calls) > 0 {
				if err := a.processToolCalls(ctx, calls, tools, yield); err != nil {
					a.endRun()
					if err != errYieldStopped {
						yield(Message{}, err)
					}
					return
				}
			}

			// Atomic exit-or-continue: drain pending input and decide
			// whether to keep looping under a single critical section.
			// Doing both under the same lock prevents a concurrent Send from
			// queueing input between an unlocked drain and a return — which
			// would strand that input with no loop left to process it. The
			// drain itself is also a safe injection point: every tool_call
			// above is paired with a tool_result, so appending user
			// messages here cannot split the (tool_call, tool_result) pair
			// the Responses API requires to be contiguous.
			a.queueMu.Lock()
			queued := a.pendingInput
			a.pendingInput = nil
			if len(queued) == 0 && len(calls) == 0 {
				a.running = false
				a.queueMu.Unlock()
				return
			}
			a.queueMu.Unlock()

			for _, in := range queued {
				a.Messages = append(a.Messages, userMessage(in))
			}

			// Proactive compaction: if the just-completed turn already filled
			// most of the context window, the next iteration (current
			// messages + assistant response + appended tool results) will be
			// even larger. Summarize older history now to avoid hitting the
			// hard limit mid-task and paying for one wasted round-trip via
			// the reactive recovery path above.
			if a.shouldCompactProactively(resp.usage.InputTokens) {
				a.compactMessages(ctx)
			}
		}
	}
}

// endRun releases the running slot and discards any leftover queued input.
// Called from every non-clean exit path in Send's loop (errors, max turns,
// recovered panics); the clean exit clears the same state inline while
// already holding queueMu.
func (a *Agent) endRun() {
	a.queueMu.Lock()
	a.running = false
	a.pendingInput = nil
	a.queueMu.Unlock()
}

func (a *Agent) shouldCompactProactively(lastInputTokens int64) bool {
	if lastInputTokens <= 0 {
		return false
	}

	window := a.Config.ContextWindow
	if window < 0 {
		return false
	}
	if window == 0 {
		window = DefaultContextWindow
	}

	reserve := a.Config.ReserveTokens
	if reserve <= 0 {
		reserve = DefaultReserveTokens
	}

	return lastInputTokens > int64(window-reserve)
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
	if len(calls) > 1 && a.allReadOnly(calls, tools) {
		return a.processToolCallsParallel(ctx, calls, tools, yield)
	}

	return a.processToolCallsSequential(ctx, calls, tools, yield)
}

func (a *Agent) processToolCallsSequential(ctx context.Context, calls []ToolCall, tools []tool.Tool, yield func(Message, error) bool) error {
	for _, tc := range calls {
		if !yield(toolCallMessage(tc), nil) {
			return errYieldStopped
		}

		resultMsg := toolResultMessage(tc, a.runSingleToolCall(ctx, tc, tools))
		a.Messages = append(a.Messages, resultMsg)

		if !yield(resultMsg, nil) {
			return errYieldStopped
		}
	}

	return nil
}

func (a *Agent) processToolCallsParallel(ctx context.Context, calls []ToolCall, tools []tool.Tool, yield func(Message, error) bool) error {
	for _, tc := range calls {
		if !yield(toolCallMessage(tc), nil) {
			return errYieldStopped
		}
	}

	type completion struct {
		index  int
		result string
	}

	results := make([]string, len(calls))
	ch := make(chan completion, len(calls))

	for i, tc := range calls {
		go func(i int, tc ToolCall) {
			ch <- completion{i, a.runSingleToolCall(ctx, tc, tools)}
		}(i, tc)
	}

	stopped := false
	for range calls {
		c := <-ch
		results[c.index] = c.result

		if !stopped && !yield(toolResultMessage(calls[c.index], c.result), nil) {
			stopped = true
		}
	}

	for i, tc := range calls {
		a.Messages = append(a.Messages, toolResultMessage(tc, results[i]))
	}

	if stopped {
		return errYieldStopped
	}

	return nil
}

func toolCallMessage(tc ToolCall) Message {
	return Message{
		Role:    RoleAssistant,
		Content: []Content{{ToolCall: &ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args}}},
	}
}

func toolResultMessage(tc ToolCall, result string) Message {
	return Message{
		Role: RoleAssistant,
		Content: []Content{{ToolResult: &ToolResult{
			ID:      tc.ID,
			Name:    tc.Name,
			Args:    tc.Args,
			Content: result,
		}}},
	}
}

func (a *Agent) runSingleToolCall(ctx context.Context, tc ToolCall, tools []tool.Tool) string {
	timeout := a.ToolTimeout
	if timeout == 0 {
		timeout = DefaultToolTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	hc := tool.ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args}

	var result string

	for _, h := range a.Hooks.PreToolUse {
		r, err := h(ctx, hc)

		if err != nil {
			result = fmt.Sprintf("error: %v", err)
			break
		}

		if r != "" {
			result = r
			break
		}
	}

	if result == "" {
		result = a.executeTool(ctx, tc, tools)
	}

	// Post hooks form a transform chain: each hook's return value replaces
	// the running result unconditionally. Hooks that want pass-through must
	// return the input `result` verbatim — see hook.PostToolUse doc.
	for _, h := range a.Hooks.PostToolUse {
		r, err := h(ctx, hc, result)

		if err != nil {
			result = fmt.Sprintf("error: %v", err)
			break
		}

		result = r
	}

	return result
}

func (a *Agent) executeTool(ctx context.Context, tc ToolCall, tools []tool.Tool) string {
	t := findTool(tc.Name, tools)

	if t == nil {
		return fmt.Sprintf("error: unknown tool %s", tc.Name)
	}

	args := make(map[string]any)

	if tc.Args != "" {
		if err := json.Unmarshal([]byte(tc.Args), &args); err != nil {
			return fmt.Sprintf("error: failed to parse arguments: %v", err)
		}
	}

	result, err := t.Execute(ctx, args)

	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	return result
}

func (a *Agent) allReadOnly(calls []ToolCall, tools []tool.Tool) bool {
	for _, tc := range calls {
		t := findTool(tc.Name, tools)
		if t == nil || t.Effect == nil {
			return false
		}

		var args map[string]any
		if tc.Args != "" {
			if err := json.Unmarshal([]byte(tc.Args), &args); err != nil {
				return false
			}
		}

		if t.Effect(args) != tool.EffectReadOnly {
			return false
		}
	}

	return true
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
	return Message{
		Role:    RoleUser,
		Content: input,
	}
}
