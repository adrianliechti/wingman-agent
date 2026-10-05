package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

func sessionResumeError(err error) error {
	// The Go SDK recognizes RequestError only as a direct value, so wrapping
	// it would lose the code and structured data when it reaches the client.
	if _, ok := err.(*acp.RequestError); ok {
		return err
	}
	return fmt.Errorf("thread/resume: %w", err)
}

// Only unwrap the known service envelope; arbitrary JSON may contain useful
// diagnostics and must stay intact.
func readableServiceErrorMessage(text string) string {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &envelope) != nil || !onlyErrorKeys(envelope, "type", "status", "error") {
		return text
	}
	var kind string
	var status int
	var detail map[string]json.RawMessage
	if json.Unmarshal(envelope["type"], &kind) != nil || kind != "error" ||
		json.Unmarshal(envelope["status"], &status) != nil || status < 400 || status > 599 ||
		json.Unmarshal(envelope["error"], &detail) != nil || !onlyErrorKeys(detail, "type", "message", "code", "param") {
		return text
	}
	var message string
	if json.Unmarshal(detail["type"], &kind) != nil || json.Unmarshal(detail["message"], &message) != nil || strings.TrimSpace(message) == "" {
		return text
	}
	switch kind {
	case "invalid_request_error", "server_error", "rate_limit_error", "insufficient_quota", "authentication_error", "permission_error", "not_found_error", "conflict_error", "overloaded_error":
	default:
		return text
	}
	for _, key := range []string{"code", "param"} {
		if raw, ok := detail[key]; ok {
			var value *string
			if json.Unmarshal(raw, &value) != nil {
				return text
			}
		}
	}
	return message
}

func onlyErrorKeys(fields map[string]json.RawMessage, keys ...string) bool {
	for field := range fields {
		found := false
		for _, key := range keys {
			found = found || field == key
		}
		if !found {
			return false
		}
	}
	return true
}
