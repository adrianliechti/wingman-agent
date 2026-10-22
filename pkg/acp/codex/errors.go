package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/coder/acp-go-sdk"
)

type workingDirectory struct {
	path string
	info os.FileInfo
}

func newWorkingDirectory(dir string) workingDirectory {
	if dir == "" {
		dir, _ = os.Getwd()
	} else if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	info, err := os.Stat(dir)
	if err != nil {
		return workingDirectory{}
	}
	return workingDirectory{path: dir, info: info}
}

// A directory recreated at the same path counts too: the process still runs in the deleted one.
func (w workingDirectory) replaced() bool {
	if w.info == nil {
		return false
	}
	info, err := os.Stat(w.path)
	return err != nil || !os.SameFile(w.info, info)
}

var configLoadErrorPattern = regexp.MustCompile(`(?i)\bload(ing)? config`)

// Codex loads project config from its process directory, so deleting it fails every config load with a bare ENOENT.
func explainConfigLoadError(err error, dir workingDirectory) error {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || !configLoadErrorPattern.MatchString(rpcErr.Message) ||
		!strings.Contains(rpcErr.Message, "No such file or directory") || !dir.replaced() {
		return err
	}
	return &rpcError{
		Code:    rpcErr.Code,
		Message: fmt.Sprintf("Codex cannot read its configuration: its working directory %s was deleted or replaced after the agent started. Restart the agent.", dir.path),
		Data:    rpcErr.Data,
	}
}

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
