package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

// Hook stops interrupt the turn without being reported as provider failures.
type hookStopError string

func (e hookStopError) Error() string { return string(e) }

// appendInputs applies the same prompt hooks to initial and queued user input.
// Hidden harness notices do not represent a new user prompt. Process the whole
// batch so rejecting one prompt does not discard other accepted inputs.
func (a *Agent) appendInputs(ctx context.Context, inputs ...Message) error {
	var messages []Message
	var inputErrors error
	for _, message := range inputs {
		var hookContext []string
		var inputErr error
		if !message.Hidden {
			for _, h := range a.Hooks.UserPromptSubmit {
				out, err := h(ctx, contentText(message.Content))
				if err != nil {
					inputErr = err
					break
				}
				if out.Block || out.Stop {
					if out.Reason == "" {
						out.Reason = "prompt blocked by hook"
					}
					inputErr = errors.New(out.Reason)
					break
				}
				hookContext = append(hookContext, out.AdditionalContext...)
			}
		}
		if inputErr != nil {
			inputErrors = errors.Join(inputErrors, inputErr)
			continue
		}
		messages = append(messages, message)
		if len(hookContext) > 0 {
			messages = append(messages, hiddenContextMessage(strings.Join(hookContext, "\n\n")))
		}
	}
	return errors.Join(inputErrors, a.appendMessages(messages...))
}

func (a *Agent) takePendingInput() []Message {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	queued := a.pendingInput
	a.pendingInput = nil
	return queued
}

func (a *Agent) completeWithRetry(ctx context.Context, turnID string, req *request, yield func(Message, error) bool) (resp *response, err error) {
	captureContent := a.Telemetry.CapturesMessageContent()
	inferenceRequest := telemetry.InferenceRequest{
		Model:          req.model,
		ConversationID: conversationID(ctx, a.CacheKey),
		Streaming:      true,
		ReasoningLevel: req.effort,
	}
	if captureContent {
		inferenceRequest.Content = telemetryInferenceContent(req.messages, req.instructions, req.tools)
	}
	inferenceCtx, inference := a.Telemetry.StartInference(ctx, inferenceRequest)
	defer func() { inference.End(streamingInferenceResult(resp, err, captureContent)) }()

	for attempt := 0; ; attempt++ {
		resp, err = a.completeRun(inferenceCtx, turnID, req, yield)
		if err == nil || errors.Is(err, errYieldStopped) {
			return resp, err
		}
		if ctx.Err() != nil {
			return resp, ctx.Err()
		}
		if attempt >= maxStreamRetries || !isRecoverableError(err) {
			return resp, err
		}
		if streamOutputStarted(err) && !EmitStreamEvent(ctx, StreamEventReset) {
			// Retry only when the consumer can retract the failed attempt.
			return resp, err
		}

		if isContextOverflowError(err) {
			if err := a.compactWithHooks(ctx, true); err != nil {
				return resp, err
			}
			req.messages = a.requestMessages()
			if captureContent {
				inference.SetContent(telemetryInferenceContent(req.messages, req.instructions, req.tools))
			}
		} else if !waitForRetry(ctx, time.Duration(attempt+1)*2*time.Second) {
			return resp, ctx.Err()
		}
		if ctx.Err() != nil {
			return resp, ctx.Err()
		}
	}
}

// Both proactive compaction and overflow recovery run the same hook lifecycle.
func (a *Agent) compactWithHooks(ctx context.Context, truncateOnFailure bool) error {
	if outcome := a.runPreCompact(ctx, "auto"); outcome.Stop {
		return hookStopError("compaction stopped by hook")
	}
	compacted, err := a.compactMessages(ctx, truncateOnFailure)
	if err != nil || !compacted {
		return err
	}
	if outcome := a.runPostCompact(ctx, "auto"); outcome.Stop {
		return hookStopError("post-compaction hook stopped the turn")
	}
	outcome, err := a.runSessionStartHooks(ctx, "compact")
	if err != nil {
		return err
	}
	if outcome.Stop {
		return hookStopError("session-start hook stopped the compacted turn")
	}
	return nil
}
