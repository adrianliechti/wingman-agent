package agent

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"golang.org/x/net/http/httpguts"
)

type RetryInfo struct {
	Attempt     int    `json:"attempt"`
	Reason      string `json:"reason"`
	DelayMillis int64  `json:"delayMillis"`
}

var retryDelayPattern = regexp.MustCompile(`(?i)\btry again in\s*([0-9]+(?:\.[0-9]+)?)\s*(ms|seconds?|s)\b`)

func retryPolicy(err error, attempt int, now time.Time) (string, time.Duration) {
	if isContextOverflowError(err) {
		return "Context window full; summarizing", 0
	}
	if isReasoningReplayError(err) {
		return "Provider rejected reasoning history; recovering", 0
	}
	delay := min(30*time.Second, 2*time.Second<<min(attempt, 4))
	delay = delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
	reason := "Connection interrupted"
	retryAfter := ""
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		reason = "Provider temporarily unavailable"
		if apiErr.StatusCode == 429 {
			reason = "Provider rate limit"
		}
		if apiErr.Response != nil {
			retryAfter = apiErr.Response.Header.Get("Retry-After")
		}
	}
	if responseErr, ok := errors.AsType[*responseFailure](err); ok {
		retryAfter = responseErr.retryAfter
	}
	headerDelay, hasHeaderDelay := parseRetryAfter(retryAfter, now)
	if hasHeaderDelay {
		delay = max(delay, headerDelay)
	}
	code, message := providerErrorDetails(err)
	if code == "rate_limit_exceeded" || code == "slow_down" {
		reason = "Provider rate limit"
		// Valid HTTP or streamed headers take precedence over message advice.
		// Only throttling messages supply fallback hints; backoff stays a floor.
		if match := retryDelayPattern.FindStringSubmatch(message); !hasHeaderDelay && len(match) == 3 {
			unit := strings.ToLower(match[2])
			if strings.HasPrefix(unit, "second") {
				unit = "s"
			}
			if hint, err := time.ParseDuration(match[1] + unit); err == nil {
				delay = max(delay, hint)
			}
		}
	}
	return reason, delay
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if !httpguts.ValidHeaderFieldValue(value) {
		return 0, false
	}
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
		return time.Duration(seconds) * time.Second, true
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now)), true
	}
	return 0, false
}

// Streamed Responses errors carry headers outside the SDK's typed schema.
func streamedRetryAfter(raw string) string {
	var headers map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &headers) != nil {
		return ""
	}
	for name, value := range headers {
		if !strings.EqualFold(name, "Retry-After") {
			continue
		}
		var header string
		if json.Unmarshal(value, &header) == nil {
			return header
		}
		// JSON numeric header values are also accepted by Codex.
		var number json.Number
		if json.Unmarshal(value, &number) == nil {
			return number.String()
		}
	}
	return ""
}
