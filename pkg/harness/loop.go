package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

// Hook stops interrupt the turn without being reported as provider failures.
type hookStopError string

func (e hookStopError) Error() string { return string(e) }

// appendInputs applies the same prompt hooks to initial and queued user input.
// Hidden harness notices do not represent a new user prompt. Process the whole
// batch so rejecting one prompt does not discard other accepted inputs.
func (a *Agent) appendInputs(ctx context.Context, inputs ...Message) error {
	if len(inputs) == 0 {
		return nil
	}
	ctx = hook.WithRuntime(ctx, a.hookRuntime(ctx))
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
		resp, err = a.completeRun(ctx, inferenceCtx, turnID, req, yield)
		_, hookStopped := errors.AsType[hookStopError](err)
		if err == nil || errors.Is(err, errYieldStopped) || hookStopped {
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
		reason, delay := retryPolicy(err, attempt, time.Now())
		if handlers, ok := ctx.Value(streamEventHandlersKey{}).(StreamEventHandlers); ok && handlers.Retry != nil {
			handlers.Retry(RetryInfo{Attempt: attempt + 1, Reason: reason, DelayMillis: delay.Milliseconds()})
		}

		if isContextOverflowError(err) {
			if err := a.compactWithHooks(ctx, req, "auto", ""); err != nil {
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
		} else if !waitForRetry(ctx, delay) {
			return resp, ctx.Err()
		}
		// Completed items and tool results survive stream failures. Rebuild
		// every retry from that history, as with compaction/replay recovery.
		if repairErr := a.repairToolHistory(req.preserveReasoning); repairErr != nil {
			return resp, repairErr
		}
		req.messages = a.requestMessages()
		if captureContent {
			inference.SetContent(telemetryInferenceContent(req.messages, req.instructions, req.tools))
		}
		if ctx.Err() != nil {
			return resp, ctx.Err()
		}
	}
}

// Manual, proactive and overflow compaction run the same hook lifecycle.
func (a *Agent) compactWithHooks(ctx context.Context, req *request, trigger, focus string) error {
	if outcome := runCompactHooks(ctx, trigger, a.Hooks.PreCompact); outcome.Stop {
		return hookStopError("compaction stopped by hook")
	}
	if err := a.compactMessages(ctx, req, focus); err != nil {
		return err
	}
	if outcome := runCompactHooks(ctx, trigger, a.Hooks.PostCompact); outcome.Stop {
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

func runCompactHooks[H ~func(context.Context, string) (hook.Outcome, error)](ctx context.Context, trigger string, hooks []H) hook.Outcome {
	var combined hook.Outcome
	for _, h := range hooks {
		outcome, err := h(ctx, trigger)
		if err == nil && outcome.Stop && !combined.Stop {
			combined = outcome
		}
	}
	return combined
}

func (a *Agent) buildStepRequest(ctx context.Context, nativeContinuation bool) (context.Context, *request, error) {
	stepCtx, req, err := a.newStepRequest(ctx, nativeContinuation)
	if err != nil {
		return stepCtx, nil, err
	}
	if err := a.prepareRequest(stepCtx, req); err != nil {
		return stepCtx, nil, err
	}
	return stepCtx, req, nil
}

func (a *Agent) newStepRequest(ctx context.Context, nativeContinuation bool) (context.Context, *request, error) {
	// Keep request settings with its hooks and tools, even if the user
	// changes settings while the response is streaming.
	stepRuntime := a.hookRuntime(ctx)
	stepCtx := hook.WithRuntime(ctx, stepRuntime)

	instructions := ""
	if a.Instructions != nil {
		instructions = a.Instructions()
	}

	// Policy stays live even when the catalog is pinned for this turn.
	tools := a.AvailableTools()
	var outputSchema map[string]any
	if schema, ok := OutputSchemaFromContext(ctx); ok {
		outputSchema = schema
	}
	if outputSchema != nil {
		// Structured output is a finalization pass. Keeping tools enabled
		// lets some providers apply a tool's argument schema to ordinary
		// text deltas instead of producing the requested final document.
		tools = nil
	}
	allowFinish := outputSchema == nil && a.RequireFinish != nil
	requireFinish := allowFinish && a.RequireFinish(stepRuntime.Model)
	if allowFinish {
		for _, t := range tools {
			if t.Name == finishToolName {
				return stepCtx, nil, fmt.Errorf("agent: tool name %q is reserved by RequireFinish", finishToolName)
			}
		}
	}
	if requireFinish {
		tools = append(slices.Clone(tools), finishTool())
		if instructions != "" {
			instructions += "\n\n"
		}
		instructions += finishInstructions
	}

	req := &request{
		model:             stepRuntime.Model,
		effort:            stepRuntime.ReasoningEffort,
		instructions:      instructions,
		cacheKey:          a.CacheKey,
		messages:          a.requestMessages(),
		tools:             tools,
		outputSchema:      outputSchema,
		requireFinish:     requireFinish,
		allowFinish:       allowFinish,
		preserveReasoning: nativeContinuation,
	}
	return stepCtx, req, nil
}

// finishStep evaluates Stop hooks when a step has no pending follow-up or
// queued input, and closes queue admission (a.finishing = true) if the turn
// is ready to terminate.
func (a *Agent) finishStep(ctx, stepCtx context.Context, lastAnswer string, needsFollowUp, incomplete bool, completionErr error, completion *completionState, stopHookActive *bool) (done bool, err error) {
	a.queueMu.Lock()
	hasPendingInput := len(a.pendingInput) > 0
	a.queueMu.Unlock()
	if completionErr == nil && !needsFollowUp && !hasPendingInput && !incomplete {
		outcome := a.runStopHooks(stepCtx, lastAnswer, *stopHookActive)
		if outcome.Stop {
			reason := outcome.Reason
			if reason == "" {
				reason = "turn stopped by hook"
			}
			return true, hookStopError(reason)
		}
		if outcome.Block {
			*stopHookActive = true
			needsFollowUp = true
			*completion = completionState{}
			reason := outcome.Reason
			if reason == "" {
				reason = "A Stop hook requested another pass."
			}
			if err := a.appendMessages(hiddenContextMessage(reason)); err != nil {
				return true, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}

	// Stop hooks run without queueMu. Recheck for newly accepted input
	// and close admission under the same lock before ending the turn.
	a.queueMu.Lock()
	// Already accepted steering gets its own chance to finish, including
	// input received during the last automatic continuation.
	finished := !needsFollowUp && len(a.pendingInput) == 0
	if finished {
		a.finishing = true
	}
	a.queueMu.Unlock()
	if finished {
		return true, completionErr
	}
	return false, nil
}

// Check before every request, including the first request of a follow-up turn.
// Rewriting historical results has a cache cost, so do it only under pressure.
func (a *Agent) prepareRequest(ctx context.Context, req *request) error {
	if err := a.normalizeRequest(req); err != nil {
		return err
	}
	overshoot := a.compactionOvershoot(req.model, a.requestTokens(req))
	if overshoot <= 0 {
		return nil
	}
	freed, err := a.compressContext(overshoot)
	if err != nil {
		return err
	}
	if int64(freed) < overshoot {
		if err := a.compactWithHooks(ctx, req, "auto", ""); err != nil {
			return err
		}
	}
	req.messages = a.requestMessages()
	return nil
}

func (a *Agent) normalizeRequest(req *request) error {
	if err := a.dropIncompatibleReasoning(req); err != nil {
		return err
	}
	return a.dropIncompatibleFinishReminders(req)
}
