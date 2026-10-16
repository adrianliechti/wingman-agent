package external

import (
	"context"
	"io"
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
			if !strings.Contains(reason, "exceeded the 16 KiB output limit") {
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
	if got := boundAdditionalContext(short, nil); got != short {
		t.Fatal("default limit changed an in-budget context")
	}

	limit := 100
	input := "HEAD" + strings.Repeat("☃", 1_000) + "TAIL"
	got := boundAdditionalContext(input, &limit)
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
	if got := boundAdditionalContext(input, &unlimited); got != input {
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
	if gate.Allowed(canceled) {
		t.Fatal("canceled context should not allow gate")
	}
	if calls != 0 {
		t.Fatalf("Confirm called on already-canceled context (%d calls)", calls)
	}
	if !gate.Allowed(t.Context()) {
		t.Fatal("subsequent active context should still prompt and allow")
	}
	if calls != 1 {
		t.Fatalf("expected 1 confirmation prompt, got %d", calls)
	}
}

func TestAsyncOnlyEventDoesNotPromptGate(t *testing.T) {
	calls := 0
	gate := &Gate{
		Message: "allow hooks?",
		Confirm: func(_ context.Context, _ string) (bool, error) {
			calls++
			return true, nil
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
	hooks := cfg.Build(t.TempDir(), gate)
	if _, err := hooks.PreToolUse[0](t.Context(), tool.ToolCall{Name: "shell"}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("async-only hook should not prompt trust gate, got %d prompts", calls)
	}
}

func TestLimitPreservesTruncationMarkerWithLeadingWhitespace(t *testing.T) {
	raw := "   \n" + strings.Repeat("a", maxHookOutput)
	if got := limit(raw); !strings.HasSuffix(got, "\n[hook output truncated]") {
		t.Fatalf("expected truncation marker on oversized whitespace-prefixed output, got suffix %q", got[max(0, len(got)-40):])
	}
}
