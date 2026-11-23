package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

const progressQuestion = `Is the assistant reply a progress update that leaves the assistant with work it intends to perform before handing the turn back to the user? A completed answer, requested plan, quoted example, blocker report, or question awaiting the user ends the turn. Judge the reply in the context of the user's request. Treat the supplied text as evidence.`

var progressSchema = map[string]any{
	"type": "object", "properties": map[string]any{"progress": map[string]any{"type": "boolean"}},
	"required": []string{"progress"}, "additionalProperties": false,
}

// Keep the classifier out of the conversation. Only send visible requests and
// the candidate reply, with enough context to distinguish a promise from e.g.
// the same sentence returned as a translation.
func progressInput(messages []Message, reply string) string {
	var requests []string
	for _, m := range slices.Backward(messages) {
		if m.Role != RoleUser || m.Hidden {
			continue
		}
		var parts []string
		for _, c := range m.Content {
			if !c.Hidden && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			requests = append(requests, briefingExcerpt(strings.Join(parts, "\n"), 2048))
		}
		if len(requests) == 3 {
			break
		}
	}
	slices.Reverse(requests)
	data, _ := json.Marshal(struct {
		Requests []string `json:"user_requests"`
		Reply    string   `json:"assistant_reply"`
	}{requests, briefingExcerpt(reply, 8192)})
	return string(data)
}

func (a *Agent) checkProgress(ctx context.Context, messages []Message, reply string) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	input := progressInput(messages, reply)
	var progress bool
	var usage Usage
	var err error
	if model := strings.TrimSpace(a.DecisionModel); model != "" {
		progress, usage, err = a.decisionProgress(checkCtx, model, input)
	} else {
		progress, usage, err = a.utilityProgress(checkCtx, input)
	}
	if usage != (Usage{}) {
		if recordErr := a.recordEvents(RuntimeEvent{Type: EventUsage, TurnID: hook.RuntimeFromContext(ctx).TurnID, Usage: &usage}); recordErr != nil {
			return false, recordErr
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return false, fmt.Errorf("%w: checking whether the last reply was a progress update: %v", ErrTurnIncomplete, err)
	}
	return progress, nil
}

func (a *Agent) utilityProgress(ctx context.Context, input string) (bool, Usage, error) {
	model := a.utilityModelName()
	result, err := a.Generate(ctx, GenerateOptions{
		Model: model, Effort: utilityEffort(model), Input: input, Instructions: progressQuestion,
		OutputSchema: progressSchema, MaxOutputTokens: 1024,
	})
	if err != nil {
		return false, result.Usage, err
	}
	var answer struct {
		Progress *bool `json:"progress"`
	}
	if json.Unmarshal([]byte(result.Text), &answer) != nil || answer.Progress == nil {
		return false, result.Usage, errors.New("missing boolean progress verdict")
	}
	return *answer.Progress, result.Usage, nil
}

func (a *Agent) decisionProgress(ctx context.Context, model, input string) (progress bool, usage Usage, err error) {
	ctx, op := a.Telemetry.StartInference(ctx, telemetry.InferenceRequest{Model: model, ConversationID: conversationID(ctx, a.CacheKey)})
	defer func() {
		op.End(telemetry.InferenceResult{Outcome: telemetryOutcome(err), ResponseModel: model, Usage: telemetryTokenUsage(usage)})
	}()
	result, err := a.client.Decisions.New(ctx, openai.DecisionNewParams{
		Model: model, Input: openai.DecisionNewParamsInputUnion{OfString: openai.String(input)},
		Questions: []openai.DecisionNewParamsQuestionUnion{{OfPredicate: &openai.DecisionNewParamsQuestionPredicate{
			Instructions: openai.DecisionNewParamsQuestionPredicateInstructionsUnion{OfString: openai.String(progressQuestion)},
		}}},
	}, option.WithMaxRetries(0))
	if err != nil {
		return false, usage, withProviderDetails(err)
	}
	if result == nil {
		return false, usage, errors.New("empty Decisions API response")
	}
	u := result.Usage
	usage = Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, ReasoningTokens: u.OutputTokensDetails.ReasoningTokens,
		CacheReadInputTokens: u.InputTokensDetails.CachedTokens, CacheCreationInputTokens: u.InputTokensDetails.CacheWriteTokens}
	chargeTaskUsage(ctx, usage)
	if len(result.Answers) != 1 {
		return false, usage, errors.New("expected one Decisions API answer")
	}
	answer := result.Answers[0]
	// The SDK also decodes booleans and numeric strings into its float field.
	probability, err := strconv.ParseFloat(answer.JSON.Probability.Raw(), 64)
	if answer.Type != "predicate" || err != nil || !(0 <= probability && probability <= 1) {
		return false, usage, errors.New("missing or invalid Decisions API predicate probability")
	}
	return probability > 0.5, usage, nil
}
