package external

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestOversizedStructuredHookResponsesFailClosed(t *testing.T) {
	for _, event := range []string{"PreToolUse", "PermissionRequest", "PostToolUse", "UserPromptSubmit", "SessionStart", "SubagentStart", "Stop", "SubagentStop", "PreCompact", "PostCompact"} {
		t.Run(event, func(t *testing.T) {
			result := parseResult(event, runResult{exitCode: 0, stdout: `{"continue":false,`, stdoutTruncated: true})
			reason := result.Reason
			switch event {
			case "PreToolUse", "UserPromptSubmit":
				if !result.Block {
					t.Fatal("oversized hook did not block")
				}
			case "PermissionRequest":
				if result.permission.Behavior != hook.PermissionDeny {
					t.Fatal("oversized permission hook did not deny")
				}
				reason = result.permission.Message
			case "SubagentStart":
				if len(result.AdditionalContext) != 1 {
					t.Fatal("missing overflow feedback")
				}
				reason = result.AdditionalContext[0]
			default:
				if !result.Stop {
					t.Fatal("oversized hook did not stop")
				}
			}
			if !strings.Contains(reason, fmt.Sprintf("exceeded the %d KiB output limit", maxHookOutput/1024)) {
				t.Fatalf("missing overflow explanation: %q", reason)
			}
		})
	}
}

func TestOversizedWhitespaceDoesNotHideHookDecision(t *testing.T) {
	for _, event := range []string{"PreToolUse", "UserPromptSubmit"} {
		result := parseResult(event, runResult{exitCode: 0, stdout: "", stdoutTruncated: true})
		if !result.Block {
			t.Fatalf("%s ignored an oversized response whose prefix was all whitespace", event)
		}
	}
}

func TestOversizedPlainHookContextKeepsMarkedPreview(t *testing.T) {
	for _, event := range []string{"SessionStart", "SubagentStart", "UserPromptSubmit"} {
		result := parseResult(event, runResult{exitCode: 0, stdout: limit(strings.Repeat("plain context\n", 2000)), stdoutTruncated: true})
		if result.Block || result.Stop || len(result.AdditionalContext) != 1 || !strings.Contains(result.AdditionalContext[0], "truncated") {
			t.Fatalf("%s plain context: %+v", event, result)
		}
	}
}

func TestOversizedStdoutDoesNotDiscardExitCodeDenial(t *testing.T) {
	result := parseResult("PreToolUse", runResult{exitCode: 2, stdout: "oversized", stdoutTruncated: true, stderr: "denied by policy"})
	if !result.Block || result.Reason != "denied by policy" {
		t.Fatalf("lost explicit denial: %+v", result)
	}
}

func TestCappedBufferDrainsAndMarksTruncation(t *testing.T) {
	var output cappedBuffer
	output.limit = maxHookOutput + 1

	input := strings.Repeat("x", maxHookOutput*3)
	written, err := output.Write([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if written != len(input) {
		t.Fatalf("Write returned %d bytes, want %d", written, len(input))
	}
	if output.Len() != maxHookOutput+1 {
		t.Fatalf("retained %d bytes, want %d", output.Len(), maxHookOutput+1)
	}
	if got := limit(output.String()); !strings.HasSuffix(got, "\n[hook output truncated]") {
		t.Fatalf("limited output did not report truncation: %q", got)
	}
}

func TestBoundAdditionalContextUsesDefaultAndConfiguredLimits(t *testing.T) {
	short := strings.Repeat("x", defaultAdditionalContextTokens*4)
	if got := boundAdditionalContext(short, nil, ""); got != short {
		t.Fatal("default limit changed an in-budget context")
	}

	limit := 100
	input := "HEAD" + strings.Repeat("☃", 1_000) + "TAIL"
	got := boundAdditionalContext(input, &limit, "")
	if !strings.Contains(got, "additionalContext truncated") {
		t.Fatal("configured limit did not report truncation")
	}
	if !strings.Contains(got, "HEAD") || !strings.Contains(got, "TAIL") {
		t.Fatal("configured limit did not preserve the context head and tail")
	}
	if !utf8.ValidString(got) {
		t.Fatal("configured limit produced invalid UTF-8")
	}

	unlimited := 0
	if got := boundAdditionalContext(input, &unlimited, ""); got != input {
		t.Fatal("zero limit did not preserve unlimited context")
	}
}

func TestCappedBufferLimitsExecStyleCopy(t *testing.T) {
	output := cappedBuffer{limit: maxHookOutput + 1}

	n, err := io.Copy(&output, strings.NewReader(strings.Repeat("x", maxHookOutput*3)))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(maxHookOutput*3) {
		t.Fatalf("io.Copy reported %d bytes, want %d", n, maxHookOutput*3)
	}
	if output.Len() != maxHookOutput+1 {
		t.Fatalf("retained %d bytes through io.Copy, want %d", output.Len(), maxHookOutput+1)
	}
	if _, ok := any(&output).(io.ReaderFrom); ok {
		t.Fatal("cappedBuffer exposes ReaderFrom, which bypasses the cap in os/exec")
	}
}

func TestGateCancellationDoesNotStickDeclined(t *testing.T) {
	calls := 0
	gate := &Gate{
		Message: "allow hooks?",
		Confirm: func(ctx context.Context, _ string) (bool, error) {
			calls++
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return true, nil
		},
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if gate.Allowed(canceled, true) {
		t.Fatal("canceled context should not allow gate")
	}
	if calls != 0 {
		t.Fatalf("Confirm called on already-canceled context (%d calls)", calls)
	}
	if !gate.Allowed(t.Context(), true) {
		t.Fatal("subsequent active context should still prompt and allow")
	}
	if calls != 1 {
		t.Fatalf("expected 1 confirmation prompt, got %d", calls)
	}
}

func TestAsyncOnlyEventPromptsGateOnlyWhenItRuns(t *testing.T) {
	calls := 0
	gate := &Gate{
		Message: "allow hooks?",
		Confirm: func(_ context.Context, _ string) (bool, error) {
			calls++
			return false, nil
		},
	}
	cfg := &Config{
		Hooks: Events{
			PreToolUse: []MatcherGroup{{
				Matcher: "Bash",
				Hooks:   []Handler{{Type: "command", Command: "echo ok", Async: true}},
			}},
		},
	}
	if _, err := cfg.Build(t.TempDir(), gate).PreToolUse[0](t.Context(), tool.ToolCall{Name: "shell"}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("skipped async hook prompted the trust gate %d times", calls)
	}
	async := NewAsync(nil)
	defer async.Close()
	hooks := cfg.BuildWithOptions(t.TempDir(), BuildOptions{Gate: gate, Async: async})
	if _, err := hooks.PreToolUse[0](t.Context(), tool.ToolCall{Name: "shell"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("async hook must pass the trust gate, got %d prompts", calls)
	}
}

type memoryTrust map[string]bool

func (m memoryTrust) Trusted(fingerprint string) bool { return m[fingerprint] }

func (m memoryTrust) Trust(fingerprint string) error {
	m[fingerprint] = true
	return nil
}

func TestGateRemembersFingerprintAndReasksAfterChange(t *testing.T) {
	store := memoryTrust{}
	fingerprint := "v1"
	prompts := 0
	newGate := func() *Gate {
		return &Gate{
			Message:     "allow hooks?",
			Confirm:     func(context.Context, string) (bool, error) { prompts++; return true, nil },
			Fingerprint: func() string { return fingerprint },
			Store:       store,
		}
	}
	if !newGate().Allowed(t.Context(), true) || prompts != 1 {
		t.Fatalf("first approval: prompts = %d", prompts)
	}
	gate := newGate()
	if !gate.Allowed(t.Context(), true) || prompts != 1 {
		t.Fatalf("unchanged hooks re-prompted in a new session: prompts = %d", prompts)
	}
	fingerprint = "v2"
	if !gate.Allowed(t.Context(), true) || prompts != 1 {
		t.Fatalf("a decided session re-checked its hooks: prompts = %d", prompts)
	}
	if !newGate().Allowed(t.Context(), true) || prompts != 2 {
		t.Fatalf("edited hooks did not re-prompt in the next session: prompts = %d", prompts)
	}
	if !store["v1"] || !store["v2"] {
		t.Fatalf("store = %#v", store)
	}
}

func TestGateAsksWithoutRememberingWhenFingerprintFails(t *testing.T) {
	store := memoryTrust{}
	prompts := 0
	gate := &Gate{
		Confirm:     func(context.Context, string) (bool, error) { prompts++; return true, nil },
		Fingerprint: func() string { return "" },
		Store:       store,
	}
	if !gate.Allowed(t.Context(), true) || prompts != 1 {
		t.Fatalf("unfingerprintable hooks were not asked about: prompts = %d", prompts)
	}
	if len(store) != 0 {
		t.Fatalf("approval without a fingerprint was remembered: %#v", store)
	}
}

func TestObserveOnlyEventsNeverPromptTheGate(t *testing.T) {
	prompts := 0
	gate := &Gate{Message: "allow hooks?", Confirm: func(context.Context, string) (bool, error) { prompts++; return true, nil }}
	cfg := &Config{Hooks: Events{Interrupt: []MatcherGroup{{Hooks: []Handler{{Type: "command", Command: "echo ok"}}}}}}
	cfg.Build(t.TempDir(), gate).Interrupt[0](t.Context())
	if prompts != 0 {
		t.Fatalf("Interrupt hook prompted the trust gate %d times", prompts)
	}
}

func TestConfigFingerprintTracksDeclarationsAndIdentity(t *testing.T) {
	preToolUse := func(command string) *Config {
		return &Config{Hooks: Events{PreToolUse: []MatcherGroup{{Matcher: "Bash", Hooks: []Handler{{Type: "command", Command: command}}}}}}
	}
	cfg := preToolUse("check.sh")
	base, err := cfg.Fingerprint("/repo", "/repo/.codex/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := cfg.Fingerprint("/repo", "/repo/.codex/hooks.json")
	otherRepo, _ := cfg.Fingerprint("/other", "/repo/.codex/hooks.json")
	edited := preToolUse("check.sh --fix")
	changed, _ := edited.Fingerprint("/repo", "/repo/.codex/hooks.json")
	if base != again || base == otherRepo || base == changed {
		t.Fatalf("fingerprints: base=%s again=%s other=%s changed=%s", base, again, otherRepo, changed)
	}
}

func TestContentHashDetectsScriptEditsAndIgnoresBytecode(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "hooks", "check.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("echo ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := ContentHash(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "__pycache__"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "__pycache__", "x.pyc"), []byte("bytecode"), 0o600); err != nil {
		t.Fatal(err)
	}
	if after, _ := ContentHash(root); after != before {
		t.Fatal("python bytecode changed the plugin fingerprint")
	}
	if err := os.WriteFile(script, []byte("echo pwned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if after, _ := ContentHash(root); after == before {
		t.Fatal("editing a hook script did not change the plugin fingerprint")
	}
}

func TestLimitPreservesTruncationMarkerWithLeadingWhitespace(t *testing.T) {
	raw := "   \n" + strings.Repeat("a", maxHookOutput)
	if got := limit(raw); !strings.HasSuffix(got, "\n[hook output truncated]") {
		t.Fatalf("expected truncation marker on oversized whitespace-prefixed output, got suffix %q", got[max(0, len(got)-40):])
	}
}
