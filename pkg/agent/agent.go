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

const maxStreamRetries = 3

type Agent struct {
	*Config

	Messages []Message
	Usage    Usage
	Revision uint64

	stateMu      sync.RWMutex
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

	a.appendMessages(userMessage(input))

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

		turns := 0
		cutoffNotified := false
		for {
			if maxTurns > 0 && turns >= maxTurns {
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
				cacheKey:     a.CacheKey,
				messages:     a.requestMessages(),
				tools:        tools,
			}

			resp, err := complete(ctx, a.client, req, yield)

			for attempt := 1; err != nil && attempt <= maxStreamRetries; attempt++ {
				if errors.Is(err, errYieldStopped) || ctx.Err() != nil || !isRecoverableError(err) {
					break
				}

				if isContextOverflowError(err) {
					a.compactMessages(ctx, true)
					req.messages = a.requestMessages()
				} else {
					// The SDK already retried transport errors with backoff; this
					// covers failures before streamed output begins, so back off
					// before resending.
					if !waitForRetry(ctx, time.Duration(attempt)*2*time.Second) {
						err = ctx.Err()
						break
					}
				}

				if ctx.Err() != nil {
					err = ctx.Err()
					break
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
			turns++

			a.stateMu.Lock()
			a.Usage.InputTokens += resp.usage.InputTokens
			a.Usage.CachedTokens += resp.usage.CachedTokens
			a.Usage.OutputTokens += resp.usage.OutputTokens
			if resp.usage.InputTokens > 0 {
				a.Usage.LastInputTokens = resp.usage.InputTokens
			}
			a.Messages = append(a.Messages, resp.messages...)
			a.stateMu.Unlock()

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

			// A cut-off response (max output tokens) drops in-flight items. When
			// tool calls survived, their results already drive the next round;
			// otherwise nudge the model once to resume. Only one consecutive
			// nudge. Content-filter stops are final; a continue nudge would just
			// re-trigger them.
			resumeAfterCutoff := resp.incomplete &&
				resp.incompleteReason != "content_filter" &&
				len(calls) == 0 &&
				!cutoffNotified

			if !resp.incomplete {
				cutoffNotified = false
			}

			a.queueMu.Lock()
			queued := a.pendingInput
			a.pendingInput = nil
			if len(queued) == 0 && len(calls) == 0 && !resumeAfterCutoff {
				a.running = false
				a.queueMu.Unlock()
				return
			}
			a.queueMu.Unlock()

			if resumeAfterCutoff {
				cutoffNotified = true
				a.appendMessages(cutoffNotice(resp.incompleteReason))
			}

			queuedMessages := make([]Message, 0, len(queued))
			for _, in := range queued {
				queuedMessages = append(queuedMessages, userMessage(in))
			}
			a.appendMessages(queuedMessages...)

			if a.shouldCompactProactively(model, resp.usage.InputTokens) {
				a.compactMessages(ctx, false)
			}
		}
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

func (a *Agent) endRun() {
	a.queueMu.Lock()
	queuedMessages := make([]Message, 0, len(a.pendingInput))
	for _, input := range a.pendingInput {
		queuedMessages = append(queuedMessages, userMessage(input))
	}
	a.appendMessages(queuedMessages...)
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
		// A fixed default reserve is too thin a margin on large (1M) windows —
		// it would defer compaction to ~97% of the window and lean on the
		// reactive overflow path. Keep at least a 10% headroom, matching the
		// ~90% trigger other Responses-API agents use. An explicit
		// Config.ReserveTokens is honored as-is.
		if frac := window / 10; frac > reserve {
			reserve = frac
		}
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
	for start := 0; start < len(calls); {
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
			err = a.processToolCallsSequential(ctx, calls[start:end], tools, yield)
		}

		if err != nil {
			return err
		}

		start = end
	}

	return nil
}

func (a *Agent) processToolCallsSequential(ctx context.Context, calls []ToolCall, tools []tool.Tool, yield func(Message, error) bool) error {
	for _, tc := range calls {
		if !yield(toolCallMessage(tc), nil) {
			return errYieldStopped
		}

		resultMsg := toolResultMessage(tc, a.runSingleToolCall(ctx, tc, tools))
		a.appendMessages(resultMsg)

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
		a.appendMessages(toolResultMessage(tc, results[i]))
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
		result = a.executeTool(ctx, tc, t, timeout, time.Now())
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

func (a *Agent) executeTool(ctx context.Context, tc ToolCall, t *tool.Tool, timeout time.Duration, started time.Time) string {
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
		// Rewrite only errors the context caused; a tool's own failure that
		// races a cancellation must stay visible as-is.
		switch {
		case errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled):
			return "error: interrupted — the request was canceled before this tool call finished"
		case errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded):
			if timeout > 0 && time.Since(started) >= timeout {
				return fmt.Sprintf("error: tool call aborted after exceeding its %s time limit", timeout)
			}
			return "error: tool call aborted — the request deadline expired before it finished"
		}
		return fmt.Sprintf("error: %v", err)
	}

	return result
}

func (a *Agent) isReadOnly(tc ToolCall, tools []tool.Tool) bool {
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

	return t.Effect(args) == tool.EffectReadOnly
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
