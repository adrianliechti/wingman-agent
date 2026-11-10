package harness

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
	"github.com/adrianliechti/wingman-agent/pkg/text"
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
// before its transport failed. Completed items may already be committed and
// their tools dispatched; retries resume from that retained history.
type streamFailure struct {
	err           error
	outputStarted bool
}

func (e *streamFailure) Error() string { return e.err.Error() }
func (e *streamFailure) Unwrap() error { return e.err }

// providerError restores the provider's code and message to an HTTP API
// error's text. openai-go reports only the status from (*openai.Error).Error(),
// which hides why a request failed (for example an exhausted quota).
type providerError struct {
	err *openai.Error
}

func (e *providerError) Error() string {
	text := e.err.Error()
	if e.err.Code != "" {
		text += " (" + e.err.Code + ")"
	}
	if e.err.Message != "" {
		text += ": " + e.err.Message
	}
	return text
}

func (e *providerError) Unwrap() error { return e.err }

func withProviderDetails(err error) error {
	apiErr, ok := errors.AsType[*openai.Error](err)
	if !ok || apiErr != err || (apiErr.Code == "" && apiErr.Message == "") {
		return err
	}
	return &providerError{err: apiErr}
}

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
	switch {
	case e.code != "" && e.message != "":
		return fmt.Sprintf("response failed (%s): %s", e.code, e.message)
	case e.message != "":
		return "response failed: " + e.message
	case e.code != "":
		return fmt.Sprintf("response failed (%s)", e.code)
	}
	return "response failed"
}

// streamedProviderFailure recovers the provider's error from the generic
// stream error openai-go returns for any event with a top-level "error"
// member. The Responses API nests its error object there, and some gateways
// send a bare string. Without a code or message there is nothing to classify,
// so the caller keeps treating the event as a broken stream.
func streamedProviderFailure(err error) *responseFailure {
	streamErr, ok := errors.AsType[*ssestream.StreamError](err)
	if !ok {
		return nil
	}
	var event struct {
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(streamErr.Event.Data, &event) != nil {
		return nil
	}
	var detail struct {
		Code    json.RawMessage `json:"code"`
		Type    json.RawMessage `json:"type"`
		Message json.RawMessage `json:"message"`
		Headers json.RawMessage `json:"headers"`
	}
	failure := &responseFailure{}
	if json.Unmarshal(event.Error, &detail) == nil {
		// Providers without a code name the failure class in type.
		failure.code = cmp.Or(jsonText(detail.Code), jsonText(detail.Type))
		failure.message = jsonText(detail.Message)
		failure.retryAfter = streamedRetryAfter(string(detail.Headers))
	} else {
		failure.message = jsonText(event.Error)
	}
	failure.code = cmp.Or(failure.code, jsonText(event.Code))
	failure.message = strings.TrimSpace(cmp.Or(failure.message, jsonText(event.Message)))
	if failure.code == "" && failure.message == "" {
		return nil
	}
	return failure
}

// jsonText returns a JSON string's value or a number's literal. Providers
// disagree on whether an error code is a name or an HTTP status.
func jsonText(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

const maxErrorBodyMessageBytes = 2048

// httpStatusError rebuilds the API error openai-go drops when an error
// response's "error" member is not an object, such as {"error":"unauthorized"}.
// The SDK then returns its JSON decoding error, which would be retried as a
// transport failure and hide both the status and the provider's text.
func httpStatusError(err error, res *http.Response) error {
	if err == nil || res == nil || res.StatusCode < 400 || res.Body == nil {
		return err
	}
	if _, ok := errors.AsType[*openai.Error](err); ok {
		return err
	}
	// The SDK restores a body it could read. When this read fails, the
	// transport broke while delivering the body and err already says so.
	body, readErr := io.ReadAll(res.Body)
	if readErr != nil {
		return err
	}
	res.Body = io.NopCloser(bytes.NewReader(body))

	apiErr := &openai.Error{StatusCode: res.StatusCode, Request: res.Request, Response: res}
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil {
		message := jsonText(payload.Error)
		if message == "" && string(payload.Error) != "null" {
			// Any other non-object value is still the provider's explanation.
			message = string(payload.Error)
		}
		apiErr.Message = text.HeadBytes(strings.TrimSpace(message), maxErrorBodyMessageBytes)
	}
	return apiErr
}

type request struct {
	model             string
	effort            string
	instructions      string
	cacheKey          string
	messages          []Message
	tools             []tool.Tool
	outputSchema      map[string]any
	requireFinish     bool
	allowFinish       bool
	disableTools      bool
	onOutputItem      func([]Message) error
	preserveReasoning bool

	cachedToolsPtr *tool.Tool
	cachedToolsLen int
	cachedToolsSet bool
	cachedParams   []responses.ToolUnionParam
	cachedToolsRaw []byte
}

func (r *request) compiledTools() []responses.ToolUnionParam {
	var ptr *tool.Tool
	if len(r.tools) > 0 {
		ptr = &r.tools[0]
	}
	if r.cachedToolsSet && r.cachedToolsPtr == ptr && r.cachedToolsLen == len(r.tools) {
		return r.cachedParams
	}
	r.cachedParams = toTools(r.tools)
	r.cachedToolsRaw, _ = json.Marshal(r.cachedParams)
	r.cachedToolsPtr = ptr
	r.cachedToolsLen = len(r.tools)
	r.cachedToolsSet = true
	return r.cachedParams
}

func (r *request) encodedTools() []byte {
	_ = r.compiledTools()
	return r.cachedToolsRaw
}

type response struct {
	messages []Message
	usage    Usage
	id       string
	model    string

	finishReasons []string
	stopReason    string
	endTurn       *bool
	dispatched    int

	incomplete       bool
	incompleteReason string
}

func (c *Config) complete(ctx context.Context, r *request, yield func(Message, error) bool) (*response, error) {
	params := responses.ResponseNewParams{
		Model:        r.model,
		Instructions: openai.String(r.instructions),

		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: toInput(r.messages)},

		Tools:             r.compiledTools(),
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
	var httpResponse *http.Response
	stream := c.client.Responses.NewStreaming(streamCtx, params, option.WithMaxRetries(0), option.WithResponseInto(&httpResponse))
	defer stream.Close()

	var outputItems []responses.ResponseInputItemUnionParam
	var final *responses.Response
	incomplete := false

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

	outputStarted := false
	emit := func(msg Message) bool {
		if !yield(msg, nil) {
			return false
		}
		outputStarted = true
		return true
	}

	reconcile := func(items []responses.ResponseInputItemUnionParam) error {
		// Some providers omit deltas or deliver only a prefix. Reconcile each
		// final text/refusal part against what actually reached the consumer.
		for _, item := range items {
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
					return &streamFailure{err: fmt.Errorf("final response text does not match delivered deltas for %s part %d", m.ID, i), outputStarted: outputStarted}
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
				if part.OfRefusal != nil {
					remember(streamedRefusals, key, remaining)
				} else {
					remember(streamedText, key, remaining)
				}
				telemetry.ObserveOutputChunk(ctx)
				if !emit(Message{
					Role: RoleAssistant, Phase: MessagePhase(m.Phase),
					Content: []Content{content},
				}) {
					return errYieldStopped
				}
			}
		}
		return nil
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
				if item.CallID == "" || r.handlesFinishTool(item.Name) {
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
			items := outputItemsToInput([]responses.ResponseOutputItemUnion{e.Item})
			outputItems = append(outputItems, items...)
			if r.onOutputItem != nil && len(items) > 0 {
				if err := reconcile(items); err != nil {
					return nil, err
				}
				if err := r.onOutputItem(r.outputMessages(items)); err != nil {
					return nil, err
				}
			}

		case responses.ResponseCompletedEvent:
			final = &e.Response

		case responses.ResponseIncompleteEvent:
			final = &e.Response
			incomplete = true

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
		if final != nil {
			break
		}
	}

	if err := stream.Err(); err != nil {
		if failure := streamedProviderFailure(err); failure != nil {
			failure.outputStarted = outputStarted
			return nil, failure
		}
		if streamCtx.Err() != nil && ctx.Err() == nil {
			err = fmt.Errorf("stream stalled: no events for %s", streamIdleTimeout)
		} else {
			err = httpStatusError(err, httpResponse)
		}
		return nil, &streamFailure{err: withProviderDetails(err), outputStarted: outputStarted}
	}
	if final == nil {
		return nil, &streamFailure{
			err:           io.ErrUnexpectedEOF,
			outputStarted: outputStarted,
		}
	}

	// Completed responses must not discard items already received. After a
	// cutoff, prefer the final output even when shorter: it includes partial
	// items that never received output_item.done and must survive continuation.
	if items := outputItemsToInput(final.Output); len(items) > 0 && (incomplete || len(items) >= len(outputItems)) {
		outputItems = items
	}
	if err := reconcile(outputItems); err != nil {
		return nil, err
	}
	resp := &response{
		messages:   r.outputMessages(outputItems),
		usage:      responseToUsage(*final),
		id:         final.ID,
		model:      final.Model,
		stopReason: responseStopReason(final),
		incomplete: incomplete,
	}

	status := "completed"
	if incomplete {
		status = "incomplete"
		resp.incompleteReason = final.IncompleteDetails.Reason
	} else {
		resp.endTurn = responseEndTurn(final)
	}
	resp.finishReasons = telemetryFinishReasons(status, resp.incompleteReason, resp.messages)
	return resp, nil
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

func (r *request) outputMessages(items []responses.ResponseInputItemUnionParam) []Message {
	messages := toMessages(items)
	prefix := reasoningPrefix(r)

	for i := range messages {
		if calls := extractToolCalls(messages[i : i+1]); len(calls) == 1 && r.handlesFinishTool(calls[0].Name) {
			messages[i].Hidden = true
		}
		for _, c := range messages[i].Content {
			if c.Reasoning != nil {
				c.Reasoning.Model = r.model
				c.Reasoning.Prefix = prefix
			}
		}
	}
	return messages
}
