package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// streamIdleTimeout bounds the wait for the next stream event; reasoning
// models can be quiet for minutes between items, so it is generous.
const streamIdleTimeout = 5 * time.Minute

type request struct {
	model        string
	effort       string
	instructions string
	cacheKey     string
	messages     []Message
	tools        []tool.Tool
}

type response struct {
	messages []Message
	usage    Usage

	incomplete       bool
	incompleteReason string
}

func complete(ctx context.Context, client *openai.Client, r *request, yield func(Message, error) bool) (*response, error) {
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

	if r.cacheKey != "" {
		params.PromptCacheKey = openai.String(r.cacheKey)
	}

	if r.effort != "" {
		rp := responses.ReasoningParam{}

		rp.Effort = shared.ReasoningEffort(r.effort)
		params.Reasoning = rp
	}

	// A stalled stream (no events at all) would otherwise hang the turn
	// forever; cancel it after a quiet period and let the retry loop resend.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	idle := time.AfterFunc(streamIdleTimeout, cancelStream)
	defer idle.Stop()

	stream := client.Responses.NewStreaming(streamCtx, params)

	var outputItems []responses.ResponseInputItemUnionParam
	var usageDelta Usage

	incomplete := false
	incompleteReason := ""

	for stream.Next() {
		idle.Reset(streamIdleTimeout)
		event := stream.Current()

		switch e := event.AsAny().(type) {
		case responses.ResponseTextDeltaEvent:
			msg := Message{
				Role:    RoleAssistant,
				Content: []Content{{Text: e.Delta}},
			}

			if !yield(msg, nil) {
				return nil, errYieldStopped
			}

		case responses.ResponseReasoningSummaryTextDeltaEvent:
			msg := Message{
				Role:    RoleAssistant,
				Content: []Content{{Reasoning: &Reasoning{ID: e.ItemID, Summary: e.Delta}}},
			}

			if !yield(msg, nil) {
				return nil, errYieldStopped
			}

		case responses.ResponseOutputItemDoneEvent:
			switch item := e.Item.AsAny().(type) {
			case responses.ResponseOutputMessage:
				var p responses.ResponseOutputMessageParam
				if err := json.Unmarshal([]byte(item.RawJSON()), &p); err != nil {
					return nil, fmt.Errorf("failed to parse output message: %w", err)
				}

				outputItems = append(outputItems, responses.ResponseInputItemUnionParam{
					OfOutputMessage: &p,
				})

			case responses.ResponseReasoningItem:
				var p responses.ResponseReasoningItemParam
				if err := json.Unmarshal([]byte(item.RawJSON()), &p); err != nil {
					return nil, fmt.Errorf("failed to parse reasoning item: %w", err)
				}

				outputItems = append(outputItems, responses.ResponseInputItemUnionParam{
					OfReasoning: &p,
				})

			case responses.ResponseFunctionToolCall:
				var p responses.ResponseFunctionToolCallParam
				if err := json.Unmarshal([]byte(item.RawJSON()), &p); err != nil {
					return nil, fmt.Errorf("failed to parse function call: %w", err)
				}

				outputItems = append(outputItems, responses.ResponseInputItemUnionParam{
					OfFunctionCall: &p,
				})
			}

		case responses.ResponseCompletedEvent:
			usageDelta = responseToUsage(e.Response)

		case responses.ResponseIncompleteEvent:
			// Output was cut short (e.g. max output tokens). The final response
			// carries the partial items — including a message that never got an
			// output_item.done — so prefer it over the accumulated stream items,
			// or the resume round would regenerate everything already streamed.
			usageDelta = responseToUsage(e.Response)
			incomplete = true
			incompleteReason = e.Response.IncompleteDetails.Reason
			if items := outputItemsFromResponse(e.Response); len(items) > 0 {
				outputItems = items
			}

		case responses.ResponseFailedEvent:
			if msg := e.Response.Error.Message; msg != "" {
				return nil, fmt.Errorf("response failed: %s", msg)
			}
			return nil, fmt.Errorf("response failed")

		case responses.ResponseErrorEvent:
			return nil, fmt.Errorf("response error: %s", e.Message)
		}
	}

	if err := stream.Err(); err != nil {
		if streamCtx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("stream stalled: no events for %s", streamIdleTimeout)
		}
		return nil, err
	}

	messages := toMessages(outputItems)

	for _, m := range messages {
		for _, c := range m.Content {
			if c.Reasoning != nil {
				c.Reasoning.Model = r.model
			}
		}
	}

	return &response{
		messages: messages,
		usage:    usageDelta,

		incomplete:       incomplete,
		incompleteReason: incompleteReason,
	}, nil
}

// outputItemsFromResponse converts a final Response's output into replayable
// input items. Function calls and reasoning are only usable when completed
// (truncated arguments or missing encrypted payloads are rejected on replay);
// partial message text is kept — preserving it is the point.
func outputItemsFromResponse(r responses.Response) []responses.ResponseInputItemUnionParam {
	var items []responses.ResponseInputItemUnionParam

	for _, item := range r.Output {
		switch it := item.AsAny().(type) {
		case responses.ResponseOutputMessage:
			var p responses.ResponseOutputMessageParam
			if err := json.Unmarshal([]byte(it.RawJSON()), &p); err == nil {
				items = append(items, responses.ResponseInputItemUnionParam{OfOutputMessage: &p})
			}

		case responses.ResponseReasoningItem:
			if it.Status != "completed" {
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
