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
	if len(messages) > 0 {
		// Commit current host guidance before the accepted input, together in
		// one batch. Later tool-driven changes still sync at request boundaries.
		if update, changed := a.contextInstructionsUpdate(a.requestMessages()); changed {
			messages = append([]Message{update}, messages...)
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
			if err := a.compactWithHooks(ctx, req); err != nil {
				return resp, err
			}
		} else if isReasoningReplayError(err) {
			dropped, dropErr := a.dropRejectedReasoning()
			if dropErr != nil {
				return resp, dropErr
			}
			if !dropped {
				return resp, err
			}
		} else if !waitForRetry(ctx, time.Duration(attempt+1)*2*time.Second) {
			return resp, ctx.Err()
		}
		if isContextOverflowError(err) || isReasoningReplayError(err) {
			req.messages = a.requestMessages()
			if captureContent {
				inference.SetContent(telemetryInferenceContent(req.messages, req.instructions, req.tools))
			}
		}
		if ctx.Err() != nil {
			return resp, ctx.Err()
		}
	}
}

// Both proactive compaction and overflow recovery run the same hook lifecycle.
func (a *Agent) compactWithHooks(ctx context.Context, req *request) error {
	if outcome := a.runPreCompact(ctx, "auto"); outcome.Stop {
		return hookStopError("compaction stopped by hook")
	}
	if err := a.compactMessages(ctx, req); err != nil {
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

// Check before every request, including the first request of a follow-up turn.
// Rewriting historical results has a cache cost, so do it only under pressure.
func (a *Agent) prepareRequest(ctx context.Context, req *request) error {
	if err := a.dropIncompatibleReasoning(req); err != nil {
		return err
	}
	req.messages = a.requestMessages()
	overshoot := a.compactionOvershoot(req.model, a.requestTokens(req))
	if overshoot <= 0 {
		return nil
	}
	freed, err := a.compressContext(overshoot)
	if err != nil {
		return err
	}
	if int64(freed) < overshoot {
		if err := a.compactWithHooks(ctx, req); err != nil {
			return err
		}
	}
	req.messages = a.requestMessages()
	return nil
}
