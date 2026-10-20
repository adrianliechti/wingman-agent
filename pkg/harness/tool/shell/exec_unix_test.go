//go:build !windows

package shell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestExecValidationMetadataKeepsWorkdirThroughPolling(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "package")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	m := NewExecManager(nil)
	defer m.Close()
	command := `read input; test "$input" = done`
	started, err := executeExecCommand(t.Context(), m, dir, nil, nil, map[string]any{
		"command": command, "workdir": "package", "validation": true, "wait": 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := executeExecSession(t.Context(), m, nil, map[string]any{"session_id": 1, "wait": 0})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := executeExecSession(t.Context(), m, nil, map[string]any{"session_id": 1, "input": "done\n", "wait": 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range []tool.Result{started, pending, completed} {
		if result.Metadata["session_id"] != 1 || result.Metadata["command"] != command || result.Metadata["workdir"] != subdir || result.Metadata["validation"] != true {
			t.Fatalf("lost validation attribution: %+v", result)
		}
	}
	if started.Metadata["exit_code"] != nil || pending.Metadata["exit_code"] != nil || completed.Metadata["exit_code"] != 0 {
		t.Fatalf("exit codes: started=%v, pending=%v, completed=%v", started.Metadata, pending.Metadata, completed.Metadata)
	}
}

func TestExecCommandCompletes(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	out, err := executeExecCommand(context.Background(), m, t.TempDir(), nil, nil, map[string]any{
		"command": "echo hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "hello") {
		t.Fatalf("output = %q", out.Content)
	}
	if !strings.Contains(out.Content, "Command completed") {
		t.Fatalf("output = %q", out.Content)
	}
	if len(m.sessions) != 0 {
		t.Fatal("expected no running sessions")
	}
}

func TestExecCommandCanStartInPrivateTempWithoutApproval(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()
	workspace, tempDir := t.TempDir(), t.TempDir()
	result, err := executeExecCommand(t.Context(), m, workspace, nil, &Options{TempDir: tempDir}, map[string]any{
		"command": "pwd", "workdir": tempDir,
	})
	if err != nil || result.IsError || !strings.Contains(result.Content, canonicalPath(tempDir)) {
		t.Fatalf("private temp workdir failed: %v %s", err, result.Content)
	}
}

func TestExecCommandExitCode(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	out, err := executeExecCommand(context.Background(), m, t.TempDir(), nil, nil, map[string]any{
		"command": "exit 3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "Command exited with code 3") || !out.IsError || out.Metadata["exit_code"] != 3 {
		t.Fatalf("output = %+v", out)
	}
}

func TestExecCommandBackgroundPollKill(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	ctx := context.Background()

	out, err := executeExecCommand(ctx, m, t.TempDir(), nil, nil, map[string]any{
		"command": "echo started; sleep 30",
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "started") || !strings.Contains(out.Content, "session_id 1") {
		t.Fatalf("output = %q", out.Content)
	}

	out, err = executeExecSession(ctx, m, nil, map[string]any{"session_id": 1, "wait": 0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "no new output") {
		t.Fatalf("output = %q", out.Content)
	}

	out, err = executeExecSession(ctx, m, nil, map[string]any{"session_id": 1, "kill": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "Session 1 killed") {
		t.Fatalf("output = %q", out.Content)
	}

	if _, err := executeExecSession(ctx, m, nil, map[string]any{"session_id": 1}); err == nil {
		t.Fatal("expected error for removed session")
	}
}

func TestExecSessionStdin(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	ctx := context.Background()

	out, err := executeExecCommand(ctx, m, t.TempDir(), nil, nil, map[string]any{
		"command": "cat",
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "session_id 1") {
		t.Fatalf("output = %q", out.Content)
	}

	out, err = executeExecSession(ctx, m, nil, map[string]any{"session_id": 1, "input": "hello\n", "wait": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "hello") {
		t.Fatalf("output = %q", out.Content)
	}

	out, err = executeExecSession(ctx, m, nil, map[string]any{"session_id": 1, "eof": true, "wait": 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "Command completed") {
		t.Fatalf("output = %q", out.Content)
	}
}

func TestExecCommandTTY(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	ctx := context.Background()

	out, err := executeExecCommand(ctx, m, t.TempDir(), nil, nil, map[string]any{
		"command": "[ -t 0 ] && echo isatty || echo notty",
		"tty":     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "isatty") {
		t.Fatalf("tty output = %q", out.Content)
	}

	out, err = executeExecCommand(ctx, m, t.TempDir(), nil, nil, map[string]any{
		"command": "[ -t 0 ] && echo isatty || echo notty",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "notty") {
		t.Fatalf("pipe output = %q", out.Content)
	}
}

func TestExecSessionTTYStdinEOF(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	ctx := context.Background()

	out, err := executeExecCommand(ctx, m, t.TempDir(), nil, nil, map[string]any{
		"command": "cat",
		"tty":     true,
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "session_id 1") {
		t.Fatalf("output = %q", out.Content)
	}

	out, err = executeExecSession(ctx, m, nil, map[string]any{"session_id": 1, "input": "hello\n", "wait": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "hello") {
		t.Fatalf("output = %q", out.Content)
	}

	out, err = executeExecSession(ctx, m, nil, map[string]any{"session_id": 1, "eof": true, "wait": 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "Command completed") {
		t.Fatalf("output = %q", out.Content)
	}
}

func TestExecSessionDangerousInputConfirmed(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	ctx := context.Background()
	confirmCalls := 0

	elicit := &tool.Elicitation{
		Confirm: func(ctx context.Context, message string) (bool, error) {
			confirmCalls++
			return false, nil
		},
	}

	out, err := executeExecCommand(ctx, m, t.TempDir(), elicit, nil, map[string]any{
		"command": "cat",
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "session_id 1") {
		t.Fatalf("output = %q", out.Content)
	}

	_, err = executeExecSession(ctx, m, elicit, map[string]any{"session_id": 1, "input": "sudo rm -rf /tmp/x\n"})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("dangerous stdin not denied: %v", err)
	}
	if confirmCalls != 1 {
		t.Fatalf("confirm called %d times, want 1", confirmCalls)
	}

	if _, err := executeExecSession(ctx, m, elicit, map[string]any{"session_id": 1, "input": "hello\n", "wait": 1}); err != nil {
		t.Fatalf("benign stdin failed: %v", err)
	}
	if confirmCalls != 1 {
		t.Fatalf("benign stdin prompted (confirm called %d times)", confirmCalls)
	}
}

func TestExecSessionDangerousInputCannotBeSplitAcrossWrites(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	ctx := context.Background()
	confirmCalls := 0
	var approvalMessage string
	elicit := &tool.Elicitation{
		Confirm: func(ctx context.Context, message string) (bool, error) {
			confirmCalls++
			approvalMessage = message
			return false, nil
		},
	}

	out, err := executeExecCommand(ctx, m, t.TempDir(), elicit, nil, map[string]any{
		"command": "cat",
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "session_id 1") {
		t.Fatalf("output = %q", out.Content)
	}

	if _, err := executeExecSession(ctx, m, elicit, map[string]any{"session_id": 1, "input": "r", "wait": 0}); err != nil {
		t.Fatalf("partial input failed: %v", err)
	}
	if confirmCalls != 0 {
		t.Fatalf("partial input prompted before submission (%d calls)", confirmCalls)
	}

	_, err = executeExecSession(ctx, m, elicit, map[string]any{"session_id": 1, "input": "m -rf /tmp/x\n"})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("split dangerous stdin not denied: %v", err)
	}
	if confirmCalls != 1 {
		t.Fatalf("confirm called %d times, want 1", confirmCalls)
	}
	if !strings.Contains(approvalMessage, "rm -rf /tmp/x") {
		t.Fatalf("approval did not show combined input: %q", approvalMessage)
	}
}

func TestExecSessionPendingInputCheckedBeforeEOFOrCtrlD(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	ctx := context.Background()
	confirmCalls := 0
	var approvalMessage string
	elicit := &tool.Elicitation{
		Confirm: func(ctx context.Context, message string) (bool, error) {
			confirmCalls++
			approvalMessage = message
			return false, nil
		},
	}

	out, err := executeExecCommand(ctx, m, t.TempDir(), elicit, nil, map[string]any{
		"command": "cat",
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "session_id 1") {
		t.Fatalf("output = %q", out.Content)
	}

	if _, err := executeExecSession(ctx, m, elicit, map[string]any{
		"session_id": 1,
		"input":      "rm -rf /tmp/wingman-marker",
		"wait":       0,
	}); err != nil {
		t.Fatalf("buffering input failed: %v", err)
	}
	if confirmCalls != 0 {
		t.Fatalf("unsubmitted input prompted early (%d calls)", confirmCalls)
	}

	if _, err := executeExecSession(ctx, m, elicit, map[string]any{"session_id": 1, "eof": true}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("dangerous pending input was not denied before EOF: %v", err)
	}
	if confirmCalls != 1 || !strings.Contains(approvalMessage, "rm -rf /tmp/wingman-marker") {
		t.Fatalf("EOF approval = %q (%d calls)", approvalMessage, confirmCalls)
	}

	if _, err := executeExecSession(ctx, m, elicit, map[string]any{"session_id": 1, "input": "\x04"}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("raw Ctrl-D was not denied: %v", err)
	}
	if confirmCalls != 2 {
		t.Fatalf("Ctrl-D confirm called %d times, want 2 total", confirmCalls)
	}
}

func TestExecSessionPromptTransformationAcrossStateAndWrites(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	ctx := context.Background()
	confirmCalls := 0
	var approvalMessage string
	elicit := &tool.Elicitation{
		Confirm: func(ctx context.Context, message string) (bool, error) {
			confirmCalls++
			approvalMessage = message
			return false, nil
		},
	}

	out, err := executeExecCommand(ctx, m, t.TempDir(), elicit, nil, map[string]any{
		"command": "cat",
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "session_id 1") {
		t.Fatalf("output = %q", out.Content)
	}

	for _, input := range []string{`a='$'` + "\n", `b="$a(touch /tmp/wingman-marker)"` + "\n", `echo ${b@`} {
		if _, err := executeExecSession(ctx, m, elicit, map[string]any{"session_id": 1, "input": input, "wait": 0}); err != nil {
			t.Fatalf("safe setup/partial input %q failed: %v", input, err)
		}
	}
	if confirmCalls != 0 {
		t.Fatalf("setup or partial expansion prompted early (%d calls)", confirmCalls)
	}

	if _, err := executeExecSession(ctx, m, elicit, map[string]any{"session_id": 1, "input": "P}\n"}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("split @P expansion was not denied: %v", err)
	}
	if confirmCalls != 1 || !strings.Contains(approvalMessage, `echo ${b@P}`) {
		t.Fatalf("prompt transformation approval = %q (%d calls)", approvalMessage, confirmCalls)
	}
}

func TestTerminalInputControlsFailClosedUntilLineSubmission(t *testing.T) {
	if !hasImmediateTerminalControl("\t") {
		t.Fatal("Tab can invoke programmable completion and must require approval")
	}
	if hasImmediateTerminalControl("\n\b\x7f\x15") {
		t.Fatal("modelled line-editing controls must not require approval by themselves")
	}
	if !terminalInputUncertainAfter(false, "ec\t") {
		t.Fatal("completion left terminal input classified as known")
	}
	if terminalInputUncertainAfter(true, "\n") {
		t.Fatal("line submission did not restore a known input boundary")
	}
	if !terminalInputUncertainAfter(false, "\n\t") {
		t.Fatal("control after the final line boundary did not make input uncertain")
	}
}

func TestTerminalControlApprovalsAreNeverRemembered(t *testing.T) {
	calls := 0
	elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) {
		calls++
		return true, nil
	}}
	s := &execSession{stdin: &testWriteCloser{}}
	m := NewExecManager(nil)
	defer m.Close()

	for range 3 {
		if _, err := writeSessionInput(context.Background(), m, 1, s, elicit, "\t"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("control approval called %d times, want one prompt per stateful key", calls)
	}
}

type testWriteCloser struct{ bytes.Buffer }

func (*testWriteCloser) Close() error { return nil }

func TestExecSessionCtrlCInterruptsPipeSession(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()

	ctx := context.Background()

	out, err := executeExecCommand(ctx, m, t.TempDir(), nil, nil, map[string]any{
		"command": "sleep 30",
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "session_id 1") {
		t.Fatalf("output = %q", out.Content)
	}

	out, err = executeExecSession(ctx, m, nil, map[string]any{"session_id": 1, "input": "\u0003", "wait": 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "signal: interrupt") {
		t.Fatalf("output = %q", out.Content)
	}
}

func TestExecSessionPollReportsExitedProcessWithZeroWait(t *testing.T) {
	// With wait=0 the timer and the exit channel are both ready; the poll must
	// still report the exit rather than racing into "still running".
	for range 50 {
		m := NewExecManager(nil)

		out, err := executeExecCommand(t.Context(), m, t.TempDir(), nil, nil, map[string]any{"command": "echo hi", "wait": 0})
		if err != nil {
			m.Close()
			t.Fatal(err)
		}
		if s := m.get(1); s != nil {
			<-s.done
			out, err = executeExecSession(t.Context(), m, nil, map[string]any{"session_id": 1, "wait": 0})
			if err != nil {
				m.Close()
				t.Fatal(err)
			}
		}
		m.Close()

		if !strings.Contains(out.Content, "Command completed") || strings.Contains(out.Content, "still running") {
			t.Fatalf("exited process reported as running: %q", out.Content)
		}
	}
}

func TestExecCommandZeroWaitReturnsExitedProcessInline(t *testing.T) {
	for range 50 {
		m := NewExecManager(nil)
		out, err := executeExecCommand(t.Context(), m, t.TempDir(), nil, nil, map[string]any{"command": "true", "wait": 0})
		m.Close()
		if err != nil {
			t.Fatal(err)
		}
		// Either the process had not exited yet (a session is handed back) or it
		// had, in which case the result must be the completed one.
		if !strings.Contains(out.Content, "Command completed") && !strings.Contains(out.Content, "session_id 1") {
			t.Fatalf("unexpected result: %q", out.Content)
		}
	}
}

func TestExecSessionBufferCap(t *testing.T) {
	s := &execSession{done: make(chan struct{})}

	s.Write(bytes.Repeat([]byte("a"), maxUnreadBytes+1000))

	out := s.drain()
	if !strings.Contains(out, "1000 bytes of earlier output dropped") {
		t.Fatalf("missing drop marker: %q", out[:80])
	}
	if len(out) > maxUnreadBytes+100 {
		t.Fatalf("drained %d bytes", len(out))
	}

	if s.drain() != "" {
		t.Fatal("expected empty buffer after drain")
	}
}

func TestExecRecreatesRemovedPrivateTempForSubsequentWorkdir(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()
	workspace := t.TempDir()
	privateTemp := filepath.Join(t.TempDir(), "private-temp")
	opts := &Options{TempDir: privateTemp, DisableSandbox: true}
	allow := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) { return true, nil }}

	if res, err := executeExecCommand(t.Context(), m, workspace, allow, opts, map[string]any{"command": `rmdir "$TMPDIR"`, "wait": 10}); err != nil || res.IsError {
		t.Fatalf("remove private temp: %v %s", err, res.Content)
	}
	if info, err := os.Stat(privateTemp); err != nil || !info.IsDir() {
		t.Fatalf("private temp not restored on exit: %v", err)
	}
	_ = os.RemoveAll(privateTemp)
	res, err := executeExecCommand(t.Context(), m, workspace, nil, opts, map[string]any{"command": "pwd", "workdir": privateTemp, "wait": 10})
	if err != nil || res.IsError || !strings.Contains(res.Content, privateTemp) {
		t.Fatalf("subsequent workdir in private temp: %v %s", err, res.Content)
	}
}
