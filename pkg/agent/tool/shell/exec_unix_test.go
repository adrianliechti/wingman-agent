//go:build !windows

package shell

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestExecCommandCompletes(t *testing.T) {
	m := NewExecManager()
	defer m.Close()

	out, err := executeExecCommand(context.Background(), m, t.TempDir(), nil, newApprovals(), map[string]any{
		"command": "echo hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "Command completed") {
		t.Fatalf("output = %q", out)
	}
	if m.runningCount() != 0 {
		t.Fatal("expected no running sessions")
	}
}

func TestExecCommandExitCode(t *testing.T) {
	m := NewExecManager()
	defer m.Close()

	out, err := executeExecCommand(context.Background(), m, t.TempDir(), nil, newApprovals(), map[string]any{
		"command": "exit 3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Command exited with code 3") {
		t.Fatalf("output = %q", out)
	}
}

func TestExecCommandBackgroundPollKill(t *testing.T) {
	m := NewExecManager()
	defer m.Close()

	ctx := context.Background()

	out, err := executeExecCommand(ctx, m, t.TempDir(), nil, newApprovals(), map[string]any{
		"command": "echo started; sleep 30",
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "started") || !strings.Contains(out, "session_id 1") {
		t.Fatalf("output = %q", out)
	}

	out, err = executeExecSession(ctx, m, map[string]any{"session_id": 1, "wait": 0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no new output") {
		t.Fatalf("output = %q", out)
	}

	out, err = executeExecSession(ctx, m, map[string]any{"session_id": 1, "kill": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Session 1 killed") {
		t.Fatalf("output = %q", out)
	}

	if _, err := executeExecSession(ctx, m, map[string]any{"session_id": 1}); err == nil {
		t.Fatal("expected error for removed session")
	}
}

func TestExecSessionStdin(t *testing.T) {
	m := NewExecManager()
	defer m.Close()

	ctx := context.Background()

	out, err := executeExecCommand(ctx, m, t.TempDir(), nil, newApprovals(), map[string]any{
		"command": "cat",
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session_id 1") {
		t.Fatalf("output = %q", out)
	}

	out, err = executeExecSession(ctx, m, map[string]any{"session_id": 1, "input": "hello\n", "wait": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("output = %q", out)
	}

	out, err = executeExecSession(ctx, m, map[string]any{"session_id": 1, "eof": true, "wait": 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Command completed") {
		t.Fatalf("output = %q", out)
	}
}

func TestExecCommandTTY(t *testing.T) {
	m := NewExecManager()
	defer m.Close()

	ctx := context.Background()

	out, err := executeExecCommand(ctx, m, t.TempDir(), nil, newApprovals(), map[string]any{
		"command": "[ -t 0 ] && echo isatty || echo notty",
		"tty":     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "isatty") {
		t.Fatalf("tty output = %q", out)
	}

	out, err = executeExecCommand(ctx, m, t.TempDir(), nil, newApprovals(), map[string]any{
		"command": "[ -t 0 ] && echo isatty || echo notty",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "notty") {
		t.Fatalf("pipe output = %q", out)
	}
}

func TestExecSessionTTYStdinEOF(t *testing.T) {
	m := NewExecManager()
	defer m.Close()

	ctx := context.Background()

	out, err := executeExecCommand(ctx, m, t.TempDir(), nil, newApprovals(), map[string]any{
		"command": "cat",
		"tty":     true,
		"wait":    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session_id 1") {
		t.Fatalf("output = %q", out)
	}

	out, err = executeExecSession(ctx, m, map[string]any{"session_id": 1, "input": "hello\n", "wait": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("output = %q", out)
	}

	out, err = executeExecSession(ctx, m, map[string]any{"session_id": 1, "eof": true, "wait": 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Command completed") {
		t.Fatalf("output = %q", out)
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
