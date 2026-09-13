package agent

import (
	"errors"
	"math/rand/v2"
	"net/http"
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
	return reason, delay
}
