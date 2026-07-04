package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

var errYieldStopped = errors.New("yield stopped")

type Agent struct {
	*Config

	Messages []Message
	Usage    Usage
	Revision uint64

	queueMu      sync.Mutex
	running      bool
	pendingInput [][]Content
}

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

		defer func() {
			if r := recover(); r != nil {
				a.endRun()
				panic(r)
			}
		}()

		turns := 1
		for {
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

			a.dropForeignReasoning(model)

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
					if isContextOverflowError(err) {
						a.compactMessages(ctx, true)
						req.messages = a.Messages
					} else {
						// The SDK already retried transport errors with backoff;
						// this covers mid-stream failures, so wait before resending.
						select {
						case <-time.After(2 * time.Second):
						case <-ctx.Done():
						}
					}

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
			turns += len(queued)

			if a.shouldCompactProactively(model, resp.usage.InputTokens) {
				a.compactMessages(ctx, false)
			}
		}
	}
}

func (a *Agent) endRun() {
	a.queueMu.Lock()
	a.running = false
	a.pendingInput = nil
	a.queueMu.Unlock()
}

func (a *Agent) shouldCompactProactively(model string, lastInputTokens int64) bool {
	if lastInputTokens <= 0 {
		return false
	}

	window := a.Config.ContextWindow
	if window < 0 {
		return false
	}
	if window == 0 {
		window = ContextWindowFor(model, a.Config.LargeContext)
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
	if tool.IsImageResult(result) {
		return Message{
			Role: RoleAssistant,
			Content: []Content{
				{ToolResult: &ToolResult{
					ID:      tc.ID,
					Name:    tc.Name,
					Args:    tc.Args,
					Content: "[image attached below]",
				}},
				{File: &File{Data: result}},
			},
		}
	}

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
	t := findTool(tc.Name, tools)

	timeout := a.ToolTimeout
	if timeout == 0 && t != nil {
		timeout = t.Timeout
	}
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
		result = a.executeTool(ctx, tc, t)
	}

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

func (a *Agent) executeTool(ctx context.Context, tc ToolCall, t *tool.Tool) string {
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
