package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

// GenerateOptions describes a stateless, tool-free model request. It is used
// by latency-sensitive helpers that must not inherit a chat session, tools, or
// conversation history.
type GenerateOptions struct {
	Model           string
	Effort          string
	Instructions    string
	Input           string
	OutputSchema    map[string]any
	MaxOutputTokens int64
}

// GenerateResult is the visible response plus provider-reported usage for
// independent budgeting and accounting. Usage is retained when a provider
// response is rejected; Text is populated only on success.
type GenerateResult struct {
	Text  string
	Usage Usage
}

// Generate runs one stateless Responses API request. A non-nil OutputSchema
// requests strict JSON schema output; callers remain responsible for decoding
// and semantically validating that JSON.
func (c *Config) Generate(ctx context.Context, opts GenerateOptions) (GenerateResult, error) {
	modelID := strings.TrimSpace(opts.Model)
	if modelID == "" {
		modelID = c.utilityModelName()
	}
	captureContent := c.Telemetry.CapturesMessageContent()
	inferenceRequest := telemetry.InferenceRequest{
		Model:          modelID,
		ConversationID: conversationID(ctx, c.CacheKey),
		ReasoningLevel: strings.TrimSpace(opts.Effort),
	}
	if captureContent {
		inferenceRequest.Content = telemetry.InferenceContent{
			InputMessages:      telemetryStringInput(opts.Input),
			SystemInstructions: telemetrySystemInstructions(opts.Instructions),
		}
	}
	ctx, operation := c.Telemetry.StartInference(ctx, inferenceRequest)
	params := responses.ResponseNewParams{
		Model:        modelID,
		Instructions: openai.String(opts.Instructions),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(opts.Input),
		},
		Store:      openai.Bool(false),
		Truncation: responses.ResponseNewParamsTruncationDisabled,
	}
	if opts.MaxOutputTokens > 0 {
		params.MaxOutputTokens = openai.Int(opts.MaxOutputTokens)
	}
	if opts.OutputSchema != nil {
		format := responses.ResponseFormatTextConfigParamOfJSONSchema(
			"response",
			opts.OutputSchema,
		)
		format.OfJSONSchema.Strict = openai.Bool(true)
		params.Text.Format = format
	}
	if effort := strings.TrimSpace(opts.Effort); effort != "" {
		params.Reasoning.Effort = shared.ReasoningEffort(effort)
	}

	resp, err := c.client.Responses.New(ctx, params)
	if err != nil {
		operation.End(telemetry.InferenceResult{Outcome: telemetryOutcome(err)})
		return GenerateResult{}, err
	}
	usage := responseToUsage(*resp)
	chargeTaskUsage(ctx, usage)
	err = responseStatusError(resp)
	defer func() { operation.End(inferenceResult(resp, usage, err, captureContent)) }()
	if err != nil {
		return GenerateResult{Usage: usage}, err
	}
	text := strings.TrimSpace(resp.OutputText())
	if opts.OutputSchema != nil && text != "" {
		var value any
		if err = json.Unmarshal([]byte(text), &value); err != nil {
			err = fmt.Errorf("decode structured model response: %w", err)
			return GenerateResult{Usage: usage}, err
		}
	}
	return GenerateResult{Text: text, Usage: usage}, nil
}

// Some compatible providers omit status. Any explicit status must indicate a
// completed response before helpers can use its output, even if text is present.
func responseStatusError(resp *responses.Response) error {
	switch resp.Status {
	case "", responses.ResponseStatusCompleted:
		if responseStopReason(resp) == "pause_turn" {
			return fmt.Errorf("response paused before completion")
		}
		return nil
	case responses.ResponseStatusFailed:
		return &responseFailure{code: string(resp.Error.Code), message: resp.Error.Message}
	case responses.ResponseStatusIncomplete:
		if reason := resp.IncompleteDetails.Reason; reason != "" {
			return fmt.Errorf("response incomplete: %s", reason)
		}
		return fmt.Errorf("response incomplete")
	default:
		return fmt.Errorf("response was not completed (status: %s)", resp.Status)
	}
}

// Wingman's extension preserves native boundaries that Responses status alone
// cannot express. Providers without the extension retain their existing behavior.
func responseStopReason(resp *responses.Response) string {
	var reason string
	_ = json.Unmarshal([]byte(resp.JSON.ExtraFields["stop_reason"].Raw()), &reason)
	return reason
}
