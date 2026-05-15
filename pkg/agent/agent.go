package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

var errYieldStopped = errors.New("yield stopped")

type Agent struct {
	*Config

	Messages []Message
	Usage    Usage
}

func (a *Agent) Send(ctx context.Context, input []Content) iter.Seq2[Message, error] {
	a.Messages = append(a.Messages, userMessage(input))

	maxTurns := a.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}

	return func(yield func(Message, error) bool) {
		turns := 0
		for {
			turns++
			if maxTurns > 0 && turns > maxTurns {
				yield(Message{}, ErrMaxTurnsExceeded)
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

			if len(calls) == 0 {
				return
			}

			if err := a.processToolCalls(ctx, calls, tools, yield); err != nil {
				if err != errYieldStopped {
					yield(Message{}, err)
				}
				return
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
	for _, tc := range calls {
		callMsg := Message{
			Role:    RoleAssistant,
			Content: []Content{{ToolCall: &ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args}}},
		}

		if !yield(callMsg, nil) {
			return errYieldStopped
		}

		result := a.runSingleToolCall(ctx, tc, tools)

		resultMsg := Message{
			Role: RoleAssistant,
			Content: []Content{{ToolResult: &ToolResult{
				ID:      tc.ID,
				Name:    tc.Name,
				Args:    tc.Args,
				Content: result,
			}}},
		}

		a.Messages = append(a.Messages, resultMsg)

		if !yield(resultMsg, nil) {
			return errYieldStopped
		}
	}

	return nil
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
