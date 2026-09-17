package agent

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
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
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		reason = "Provider temporarily unavailable"
		if apiErr.StatusCode == 429 {
			reason = "Provider rate limit"
		}
		if apiErr.Response != nil {
			value := strings.TrimSpace(apiErr.Response.Header.Get("Retry-After"))
			if seconds, parseErr := strconv.ParseInt(value, 10, 32); parseErr == nil && seconds >= 0 {
				delay = max(delay, time.Duration(seconds)*time.Second)
			} else if date, parseErr := http.ParseTime(value); parseErr == nil {
				delay = max(delay, date.Sub(now))
			}
		}
	}
	code, message := providerErrorDetails(err)
	if code == "rate_limit_exceeded" || code == "slow_down" {
		reason = "Provider rate limit"
		// SSE failures have no Retry-After header. Only interpret delay hints
		// on known throttling errors, and keep the jittered backoff as a floor.
		if match := retryDelayPattern.FindStringSubmatch(message); len(match) == 3 {
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
