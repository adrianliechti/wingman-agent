package agent

import (
	"errors"
	"slices"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func isRecoverableError(err error) bool {
	code, _ := providerErrorDetails(err)
	switch code {
	case "insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded":
		return false
	}

	if isContextOverflowError(err) || isReasoningReplayError(err) {
		return true
	}

	var streamErr *streamFailure
	errors.As(err, &streamErr)

	if responseErr, ok := errors.AsType[*responseFailure](err); ok {
		switch responseErr.code {
		case string(responses.ResponseErrorCodeServerError),
			string(responses.ResponseErrorCodeRateLimitExceeded),
			string(responses.ResponseErrorCodeVectorStoreTimeout), "slow_down":
			return true
		default:
			return false
		}
	}

	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		switch apiErr.StatusCode {
		case 408, 409, 429:
			return true
		default:
			return apiErr.StatusCode >= 500
		}
	}

	// Other non-API failures are retryable only when complete marked them as a
	// stream transport failure. Partial responses are not committed and their
	// tool calls are not executed, so a replay cannot duplicate side effects.
	return streamErr != nil
}

func providerErrorDetails(err error) (code, message string) {
	if responseErr, ok := errors.AsType[*responseFailure](err); ok {
		return responseErr.code, responseErr.message
	}
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		return apiErr.Code, apiErr.Message
	}
	return "", ""
}

func streamOutputStarted(err error) bool {
	if streamErr, ok := errors.AsType[*streamFailure](err); ok {
		return streamErr.outputStarted
	}
	if responseErr, ok := errors.AsType[*responseFailure](err); ok {
		return responseErr.outputStarted
	}
	return false
}

var contextOverflowMarkers = []string{
	"context length",
	"context_length",
	"context window",
	"context_window",
	"maximum context",
	"too many tokens",
	"prompt is too long",
	"input is too long",
}

// Providers can reject input either as HTTP errors or terminal SSE events.
// Only inspect structured input errors; unrelated transport failures retry
// without destructively rewriting history.
func providerInputError(err error) string {
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		switch apiErr.StatusCode {
		case 400, 413, 422:
			return strings.ToLower(apiErr.Code + " " + apiErr.Message)
		}
	}
	if responseErr, ok := errors.AsType[*responseFailure](err); ok {
		return strings.ToLower(responseErr.code + " " + responseErr.message)
	}
	return ""
}

func isContextOverflowError(err error) bool {
	msg := providerInputError(err)
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}

	return false
}

func isReasoningReplayError(err error) bool {
	msg := strings.ReplaceAll(providerInputError(err), "`", "")
	return strings.Contains(msg, "invalid_encrypted_content") ||
		strings.Contains(msg, "encrypted content could not be verified") ||
		(strings.Contains(msg, "thinking") && (strings.Contains(msg, "invalid signature") || strings.Contains(msg, "signature verification failed")))
}

// As in Codex normalization, missing results get a stable interruption notice.
// Keep the attempted call so the next model cannot mistake it for unstarted
// work. A result without a call is removed at content granularity.
func (a *Agent) repairToolHistory() error {
	messages := a.contextSnapshot()

	callIDs := make(map[string]bool)
	outputIDs := make(map[string]bool)

	for _, m := range messages {
		for _, c := range m.Content {
			if c.ToolCall != nil && c.ToolCall.ID != "" {
				callIDs[c.ToolCall.ID] = true
			}

			if c.ToolResult != nil && c.ToolResult.ID != "" {
				outputIDs[c.ToolResult.ID] = true
			}
		}
	}

	var cleaned, missing []Message
	changed := false
	for _, m := range messages {
		hasCall := false
		for _, c := range m.Content {
			hasCall = hasCall || c.ToolCall != nil
		}
		// Flush after the whole call group, before its tool results. This
		// keeps parallel calls legal on strict Anthropic-compatible backends.
		if !hasCall {
			cleaned = append(cleaned, missing...)
			missing = nil
		}
		m.Content = slices.DeleteFunc(m.Content, func(c Content) bool {
			if c.ToolResult != nil && !callIDs[c.ToolResult.ID] {
				changed = true
				return true
			}
			return false
		})
		for _, c := range m.Content {
			if call := c.ToolCall; call != nil && call.ID != "" && !outputIDs[call.ID] {
				missing = append(missing, Message{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{
					ID: call.ID, Name: call.Name, Args: call.Args, IsError: true,
					Content: "No recorded result is available for this tool call. It may have been interrupted; execution and side effects are unknown. Check the current state before retrying.",
				}}}})
				outputIDs[call.ID] = true
				changed = true
			}
		}
		if len(m.Content) > 0 {
			cleaned = append(cleaned, m)
		}
	}
	cleaned = append(cleaned, missing...)

	if pruned, ok := dropDanglingReasoning(cleaned); ok {
		cleaned = pruned
		changed = true
	}

	if !changed {
		return nil
	}
	return a.replaceContext("repair interrupted tool or reasoning history", cleaned)
}
