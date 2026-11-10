package harness

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func isRecoverableError(err error) bool {
	// A retryable provider failure must not hide a simultaneous local failure
	// (for example, an unsaved tool result) or consumer cancellation.
	if joined, ok := errors.AsType[interface {
		error
		Unwrap() []error
	}](err); ok {
		for _, cause := range joined.Unwrap() {
			if !isRecoverableError(cause) {
				return false
			}
		}
		return true
	}
	code, _ := providerErrorDetails(err)
	if isQuotaCode(code) {
		return false
	}

	if isContextOverflowError(err) || isReasoningReplayError(err) {
		return true
	}

	var streamErr *streamFailure
	errors.As(err, &streamErr)

	if responseErr, ok := errors.AsType[*responseFailure](err); ok {
		return isTransientProviderCode(responseErr.code)
	}

	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		return isRetryableStatus(apiErr.StatusCode)
	}

	// Other non-API failures are retryable only when complete marked them as a
	// stream transport failure. Retries include completed items and their
	// tool results; incomplete items are discarded.
	return streamErr != nil
}

// Spend and usage limits do not clear by waiting.
func isQuotaCode(code string) bool {
	switch code {
	case "insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded",
		"project_spend_limit_exceeded", "organization_usage_limit_exceeded":
		return true
	}
	return false
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

// In-band failures retry only for codes known to be transient; an unknown
// code fails the turn. Gateways forward the upstream provider's vocabulary,
// so this covers more than the Responses API's own closed set.
func isTransientProviderCode(code string) bool {
	switch strings.ToLower(code) {
	case string(responses.ResponseErrorCodeServerError),
		string(responses.ResponseErrorCodeRateLimitExceeded),
		string(responses.ResponseErrorCodeVectorStoreTimeout),
		"slow_down", "server_is_overloaded", "service_unavailable_error":
		return true
	// Anthropic error types, and the exception names of its Bedrock transport.
	case "overloaded_error", "api_error", "rate_limit_error", "timeout_error",
		"throttlingexception", "serviceunavailableexception", "modeltimeoutexception", "internalserverexception":
		return true
	// google.rpc status names.
	case "resource_exhausted", "unavailable", "deadline_exceeded", "internal":
		return true
	}
	// Some providers report the HTTP status as the code.
	if status, err := strconv.Atoi(code); err == nil && status < 600 {
		return isRetryableStatus(status)
	}
	return false
}

// isRateLimitCode reports in-band codes that mean the provider is throttling
// the account, as opposed to failing or being overloaded.
func isRateLimitCode(code string) bool {
	switch strings.ToLower(code) {
	case string(responses.ResponseErrorCodeRateLimitExceeded), "slow_down",
		"rate_limit_error", "throttlingexception", "resource_exhausted", "429":
		return true
	}
	return false
}

// isTerminalProviderError reports rejections that neither a smaller input nor
// another model can fix: the account is out of quota or unauthorized, or the
// provider refused the content.
func isTerminalProviderError(err error) bool {
	code, _ := providerErrorDetails(err)
	if isQuotaCode(code) {
		return true
	}
	switch strings.ToLower(code) {
	case "authentication_error", "permission_error",
		"content_policy_violation", "content_filter", "invalid_prompt",
		"cyber_policy", "bio_policy", "misalignment_policy_violation":
		return true
	}
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		return apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden
	}
	return false
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

// Each marker lists fragments that must appear in order; providers put the
// measured token counts between them. Keep them specific to input size: an
// output-limit rejection must not trigger compaction.
var contextOverflowMarkers = [][]string{
	{"context length"},
	{"context_length"},
	{"context window"},
	{"context_window"},
	{"maximum context"},
	{"too many tokens"},
	{"prompt is too long"},
	{"input is too long"},
	// Anthropic: input length and `max_tokens` exceed context limit: N + M > W
	{"exceed", "context limit"},
	// Gemini: The input token count (N) exceeds the maximum number of tokens allowed (W)
	{"input token count", "exceeds the maximum"},
	// xAI: This model's maximum prompt length is W but the request contains N tokens
	{"maximum prompt length"},
	// llama.cpp: the request exceeds the available context size
	{"exceeds the available context size"},
	// Bedrock: Too many input tokens
	{"too many input tokens"},
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
	// 413 names the failure by status: the request is too large. Proxies and
	// some providers send it without a body to match, and sending less is the
	// only remedy either way.
	if apiErr, ok := errors.AsType[*openai.Error](err); ok && apiErr.StatusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	// Throttling messages count tokens too (Bedrock: "Too many tokens, please
	// wait before trying again"). Waiting is the remedy, not a smaller context.
	if responseErr, ok := errors.AsType[*responseFailure](err); ok && isRateLimitCode(responseErr.code) {
		return false
	}
	msg := providerInputError(err)
	for _, marker := range contextOverflowMarkers {
		if containsInOrder(msg, marker) {
			return true
		}
	}

	return false
}

func containsInOrder(s string, fragments []string) bool {
	for _, fragment := range fragments {
		_, after, found := strings.Cut(s, fragment)
		if !found {
			return false
		}
		s = after
	}
	return true
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
func (a *Agent) repairToolHistory(preserveReasoning bool) error {
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

	// An explicit end_turn:false can intentionally leave reasoning at the
	// end of history. Keep it for the provider's next sampling request.
	if !preserveReasoning {
		if pruned, ok := dropDanglingReasoning(cleaned); ok {
			cleaned = pruned
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return a.replaceContext("repair interrupted tool or reasoning history", cleaned)
}
