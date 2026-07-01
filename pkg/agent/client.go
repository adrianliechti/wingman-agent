package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type request struct {
	model        string
	effort       string
	instructions string
	messages     []Message
	tools        []tool.Tool
}

type response struct {
	messages []Message
	usage    Usage
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

	if r.effort != "" {
		rp := responses.ReasoningParam{}

		rp.Effort = shared.ReasoningEffort(r.effort)
		params.Reasoning = rp
	}

	stream := client.Responses.NewStreaming(ctx, params)

	var outputItems []responses.ResponseInputItemUnionParam
	var usageDelta Usage

	for stream.Next() {
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
			// Output was cut short (e.g. max output tokens); completed items
			// and usage still count.
			usageDelta = responseToUsage(e.Response)

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
	}, nil
}
