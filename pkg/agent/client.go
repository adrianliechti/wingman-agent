package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

// streamIdleTimeout bounds the wait for the next stream event; reasoning
// models can be quiet for minutes between items, so it is generous.
const streamIdleTimeout = 5 * time.Minute

const (
	partialArgsInterval  = 100 * time.Millisecond
	partialArgsMinGrowth = 64
)

type pendingToolCall struct {
	id            string
	name          string
	args          []byte
	lastYield     time.Time
	lastYieldSize int
}

func (p *pendingToolCall) message() Message {
	presentation := NewToolPresentation(p.name, "", string(p.args), nil)
	return Message{
		Role: RoleAssistant,
		Content: []Content{{ToolCall: &ToolCall{
			ID: p.id, Name: p.name, Args: string(p.args), Partial: true,
			Presentation: presentation,
		}}},
	}
}

func (p *pendingToolCall) snapshotReady(now time.Time) bool {
	if now.Sub(p.lastYield) < partialArgsInterval {
		return false
	}
	growth := len(p.args) - p.lastYieldSize
	return growth >= max(partialArgsMinGrowth, p.lastYieldSize/4)
}

func (p *pendingToolCall) markSnapshot(now time.Time) {
	p.lastYield = now
	p.lastYieldSize = len(p.args)
}

// streamFailure records whether a streamed request produced visible output
// before its transport failed. The response is not committed and tool calls
// are not executed until a terminal event arrives, so retrying remains safe;
// outputStarted only explains why a client may have seen repeated deltas.
type streamFailure struct {
	err           error
	outputStarted bool
}

func (e *streamFailure) Error() string { return e.err.Error() }
func (e *streamFailure) Unwrap() error { return e.err }

// responseFailure preserves the machine-readable code from a terminal
// Responses API error event. Some providers report transient failures in-band
// instead of as an HTTP error, so recovery needs more than the display string.
type responseFailure struct {
	code          string
	message       string
	retryAfter    string
	outputStarted bool
}

func (e *responseFailure) Error() string {
	if e.message != "" {
		return fmt.Sprintf("response failed (%s): %s", e.code, e.message)
	}
	if e.code != "" {
		return fmt.Sprintf("response failed (%s)", e.code)
	}
	return "response failed"
}

type request struct {
	model         string
	effort        string
	instructions  string
	cacheKey      string
	messages      []Message
	tools         []tool.Tool
	outputSchema  map[string]any
	requireFinish bool
	disableTools  bool
}

type response struct {
	messages []Message
	usage    Usage
	id       string
	model    string

	finishReasons []string
	stopReason    string

	incomplete       bool
	incompleteReason string
}

func (c *Config) complete(ctx context.Context, r *request, yield func(Message, error) bool) (*response, error) {
	params := responses.ResponseNewParams{
		Model:        r.model,
		Instructions: openai.String(r.instructions),

		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: toInput(r.messages)},

		Tools:             toTools(r.tools),
		ParallelToolCalls: openai.Bool(true),

		// Encrypted reasoning lets the model resume its chain of thought across
		// tool rounds instead of re-reasoning after every result.
		Include: []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},

		Store: openai.Bool(false),
		// Overflow must surface as an error so compactMessages owns recovery;
		// "auto" would silently drop mid-conversation context server-side.
		Truncation: responses.ResponseNewParamsTruncationDisabled,
	}
	if budget := c.outputTokenBudgetFor(r.model); budget > 0 {
		params.MaxOutputTokens = openai.Int(int64(budget))
	}
	if r.disableTools {
		params.ToolChoice.OfToolChoiceMode = openai.Opt(responses.ToolChoiceOptionsNone)
	}
	if r.cacheKey != "" {
		params.PromptCacheKey = openai.String(r.cacheKey)
	}
	if m, ok := model.Find(r.model); ok && m.Verbosity != "" {
		params.Text.Verbosity = responses.ResponseTextConfigVerbosity(m.Verbosity)
	}

	if r.effort != "" {
		rp := responses.ReasoningParam{}
		rp.Effort = shared.ReasoningEffort(r.effort)

		// "auto" yields the richest summary the model supports; skipped for
		// "none" since no reasoning happens that could be summarized.
		if r.effort != "none" {
			rp.Summary = shared.ReasoningSummaryAuto
		}

		params.Reasoning = rp
	}

	if r.outputSchema != nil {
		if len(r.outputSchema) == 0 {
			format := shared.NewResponseFormatJSONObjectParam()
			params.Text.Format.OfJSONObject = &format
		} else {
			format := responses.ResponseFormatTextConfigParamOfJSONSchema(
				"response",
				r.outputSchema,
			)
			format.OfJSONSchema.Strict = openai.Bool(true)
			params.Text.Format = format
		}
	}

	// A stalled stream (no events at all) would otherwise hang the turn
	// forever; cancel it after a quiet period and let the retry loop resend.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	idle := time.AfterFunc(streamIdleTimeout, cancelStream)
	defer idle.Stop()

	// The harness owns retries and their visible recovery boundaries.
	stream := c.client.Responses.NewStreaming(streamCtx, params, option.WithMaxRetries(0))
	defer stream.Close()

	var outputItems []responses.ResponseInputItemUnionParam
	var usageDelta Usage

	pendingCalls := map[int64]*pendingToolCall{}
	type outputPart struct {
		itemID string
		index  int64
	}
	streamedText := map[outputPart]*strings.Builder{}
	streamedRefusals := map[outputPart]*strings.Builder{}
	remember := func(parts map[outputPart]*strings.Builder, key outputPart, delta string) {
		if parts[key] == nil {
			parts[key] = &strings.Builder{}
		}
		parts[key].WriteString(delta)
	}

	incomplete := false
	incompleteReason := ""
	responseID := ""
	responseModel := ""
	stopReason := ""
	outputStarted := false
	terminalEvent := false
	emit := func(msg Message) bool {
		if !yield(msg, nil) {
			return false
		}
		outputStarted = true
		return true
	}

	for stream.Next() {
		idle.Reset(streamIdleTimeout)
		telemetry.ObserveResponseChunk(ctx)
		event := stream.Current()
		switch e := event.AsAny().(type) {
		case responses.ResponseTextDeltaEvent:
			if e.Delta == "" {
				continue
			}
			remember(streamedText, outputPart{e.ItemID, e.ContentIndex}, e.Delta)
			telemetry.ObserveOutputChunk(ctx)
			msg := Message{
				Role:    RoleAssistant,
				Content: []Content{{Text: e.Delta, TextID: e.ItemID}},
			}

			if !emit(msg) {
				return nil, errYieldStopped
			}

		case responses.ResponseRefusalDeltaEvent:
			if e.Delta == "" {
				continue
			}
			remember(streamedRefusals, outputPart{e.ItemID, e.ContentIndex}, e.Delta)
			telemetry.ObserveOutputChunk(ctx)
			if !emit(Message{
				Role:    RoleAssistant,
				Content: []Content{{Refusal: e.Delta, TextID: e.ItemID}},
			}) {
				return nil, errYieldStopped
			}

		case responses.ResponseReasoningSummaryTextDeltaEvent:
			if e.Delta == "" {
				continue
			}
			telemetry.ObserveOutputChunk(ctx)
			msg := Message{
				Role:    RoleAssistant,
				Content: []Content{{Reasoning: &Reasoning{ID: e.ItemID, Part: int(e.SummaryIndex), Summary: e.Delta}}},
			}

			if !emit(msg) {
				return nil, errYieldStopped
			}

		case responses.ResponseOutputItemAddedEvent:
			switch item := e.Item.AsAny().(type) {
			case responses.ResponseFunctionToolCall:
				if item.CallID == "" || r.requireFinish && item.Name == finishToolName {
					break
				}
				pending := &pendingToolCall{
					id:            item.CallID,
					name:          item.Name,
					args:          []byte(item.Arguments),
					lastYield:     time.Now(),
					lastYieldSize: len(item.Arguments),
				}
				pendingCalls[e.OutputIndex] = pending

				if !emit(pending.message()) {
					return nil, errYieldStopped
				}
			}

		case responses.ResponseFunctionCallArgumentsDeltaEvent:
			telemetry.ObserveOutputChunk(ctx)
			if pending := pendingCalls[e.OutputIndex]; pending != nil {
				pending.args = append(pending.args, e.Delta...)

				now := time.Now()
				if pending.snapshotReady(now) {
					pending.markSnapshot(now)

					if !emit(pending.message()) {
						return nil, errYieldStopped
					}
				}
			}

		case responses.ResponseFunctionCallArgumentsDoneEvent:
			if pending := pendingCalls[e.OutputIndex]; pending != nil {
				delete(pendingCalls, e.OutputIndex)
				pending.args = []byte(e.Arguments)

				if !emit(pending.message()) {
					return nil, errYieldStopped
				}
			}

		case responses.ResponseOutputItemDoneEvent:
			outputItems = append(outputItems, outputItemsToInput([]responses.ResponseOutputItemUnion{e.Item})...)

		case responses.ResponseCompletedEvent:
			stopReason = responseStopReason(&e.Response)
			usageDelta = responseToUsage(e.Response)
			responseID = e.Response.ID
			responseModel = e.Response.Model
			terminalEvent = true
			if items := outputItemsToInput(e.Response.Output); len(items) >= len(outputItems) && len(items) > 0 {
				outputItems = items
			}

		case responses.ResponseIncompleteEvent:
			stopReason = responseStopReason(&e.Response)
			// Output was cut short (e.g. max output tokens). The final response
			// carries the partial items — including a message that never got an
			// output_item.done — so prefer it over the accumulated stream items,
			// or the resume round would regenerate everything already streamed.
			usageDelta = responseToUsage(e.Response)
			responseID = e.Response.ID
			responseModel = e.Response.Model
			incomplete = true
			incompleteReason = e.Response.IncompleteDetails.Reason
			terminalEvent = true
			if items := outputItemsToInput(e.Response.Output); len(items) > 0 {
				outputItems = items
			}

		case responses.ResponseFailedEvent:
			return nil, &responseFailure{
				code:          string(e.Response.Error.Code),
				message:       e.Response.Error.Message,
				retryAfter:    streamedRetryAfter(e.Response.Error.JSON.ExtraFields["headers"].Raw()),
				outputStarted: outputStarted,
			}

		case responses.ResponseErrorEvent:
			return nil, &responseFailure{
				code:          e.Code,
				message:       e.Message,
				outputStarted: outputStarted,
			}
		}

		// completed and incomplete carry the authoritative final response.
		// Do not wait for a redundant [DONE] marker or connection close.
		if terminalEvent {
			break
		}
	}

	if err := stream.Err(); err != nil {
		if streamCtx.Err() != nil && ctx.Err() == nil {
			err = fmt.Errorf("stream stalled: no events for %s", streamIdleTimeout)
		}
		return nil, &streamFailure{err: err, outputStarted: outputStarted}
	}
	if !terminalEvent {
		return nil, &streamFailure{
			err:           io.ErrUnexpectedEOF,
			outputStarted: outputStarted,
		}
	}

	// Some providers omit deltas or deliver only a prefix. Reconcile each
	// final text/refusal part against what actually reached the consumer.
	for _, item := range outputItems {
		m := item.OfOutputMessage
		if m == nil {
			continue
		}
		for i, part := range m.Content {
			var full string
			var sent *strings.Builder
			key := outputPart{m.ID, int64(i)}
			switch {
			case part.OfOutputText != nil:
				full, sent = part.OfOutputText.Text, streamedText[key]
			case part.OfRefusal != nil:
				full, sent = part.OfRefusal.Refusal, streamedRefusals[key]
			default:
				continue
			}
			seen := ""
			if sent != nil {
				seen = sent.String()
			}
			if !strings.HasPrefix(full, seen) {
				return nil, &streamFailure{err: fmt.Errorf("final response text does not match delivered deltas for %s part %d", m.ID, i), outputStarted: outputStarted}
			}
			remaining := strings.TrimPrefix(full, seen)
			if remaining == "" {
				continue
			}
			content := Content{TextID: m.ID}
			if part.OfRefusal != nil {
				content.Refusal = remaining
			} else {
				content.Text = remaining
			}
			telemetry.ObserveOutputChunk(ctx)
			if !emit(Message{
				Role: RoleAssistant, Phase: MessagePhase(m.Phase),
				Content: []Content{content},
			}) {
				return nil, errYieldStopped
			}
		}
	}
	messages := toMessages(outputItems)
	if r.requireFinish {
		for i := range messages {
			if calls := extractToolCalls(messages[i : i+1]); len(calls) == 1 && calls[0].Name == finishToolName {
				messages[i].Hidden = true
			}
		}
	}
	prefix := reasoningPrefix(r)

	for _, m := range messages {
		for _, c := range m.Content {
			if c.Reasoning != nil {
				c.Reasoning.Model = r.model
				c.Reasoning.Prefix = prefix
			}
		}
	}

	status := "completed"
	if incomplete {
		status = "incomplete"
	}
	finishReasons := telemetryFinishReasons(status, incompleteReason, messages)
	return &response{
		messages: messages,
		usage:    usageDelta,
		id:       responseID,
		model:    responseModel,

		finishReasons: finishReasons,
		stopReason:    stopReason,

		incomplete:       incomplete,
		incompleteReason: incompleteReason,
	}, nil
}

// outputItemsToInput applies the same replay checks to item-done events and
// final responses. Unfinished tool calls/reasoning cannot be replayed; partial
// assistant text is retained for continuity after a cutoff.
func outputItemsToInput(output []responses.ResponseOutputItemUnion) []responses.ResponseInputItemUnionParam {
	var items []responses.ResponseInputItemUnionParam

	for _, item := range output {
		switch it := item.AsAny().(type) {
		case responses.ResponseOutputMessage:
			var p responses.ResponseOutputMessageParam
			if err := json.Unmarshal([]byte(it.RawJSON()), &p); err == nil {
				items = append(items, responses.ResponseInputItemUnionParam{OfOutputMessage: &p})
			}

		case responses.ResponseReasoningItem:
			// Status is optional for reasoning; reject explicit unfinished items.
			if it.Status != "" && it.Status != "completed" {
				continue
			}
			var p responses.ResponseReasoningItemParam
			if err := json.Unmarshal([]byte(it.RawJSON()), &p); err == nil {
				items = append(items, responses.ResponseInputItemUnionParam{OfReasoning: &p})
			}

		case responses.ResponseFunctionToolCall:
			if it.Status != "completed" {
				continue
			}
			var p responses.ResponseFunctionToolCallParam
			if err := json.Unmarshal([]byte(it.RawJSON()), &p); err == nil {
				items = append(items, responses.ResponseInputItemUnionParam{OfFunctionCall: &p})
			}

		}
	}

	return items
}
