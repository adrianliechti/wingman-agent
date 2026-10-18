//go:build !windows

package shell

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestExecDangerousCommandRequiresAvailableApproval(t *testing.T) {
	for _, scenario := range []string{"missing", "empty", "review denial", "review error"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "preserve-me")
			if err := os.WriteFile(marker, []byte("user work"), 0600); err != nil {
				t.Fatal(err)
			}
			var elicit *tool.Elicitation
			want := "approval"
			calls := 0
			switch scenario {
			case "empty":
				elicit = &tool.Elicitation{}
			case "review denial", "review error":
				want = "fixture rejection"
				elicit = &tool.Elicitation{Review: func(context.Context, tool.Approval) (tool.Verdict, error) {
					calls++
					if scenario == "review error" {
						return tool.Verdict{}, errors.New(want)
					}
					return tool.Verdict{Reason: want}, nil
				}}
			}
			m := NewExecManager(nil)
			defer m.Close()
			_, err := ExecTools(m, dir, elicit, &Options{DisableSandbox: true})[0].Execute(t.Context(), map[string]any{"command": "rm -rf preserve-me", "wait": 10})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("unapproved command: %v", err)
			}
			if elicit != nil && elicit.Review != nil && calls != 1 {
				t.Fatalf("review-only approver called %d times", calls)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("unapproved command removed user work")
			}
		})
	}
}

func TestExecRechecksApprovalAfterFilesChange(t *testing.T) {
	for _, reviewer := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmation", true: "review"}[reviewer], func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "fixture")
			calls := 0
			elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) {
				if reviewer {
					t.Fatal("confirmation bypassed the reviewer")
				}
				calls++
				return calls == 1, nil
			}}
			if reviewer {
				elicit.Review = func(context.Context, tool.Approval) (tool.Verdict, error) {
					calls++
					return tool.Verdict{Allowed: calls == 1, Reason: "new user work"}, nil
				}
			}
			m := NewExecManager(nil)
			defer m.Close()
			exec := ExecTools(m, dir, elicit, &Options{DisableSandbox: true})[0]
			args := map[string]any{"command": "rm -rf fixture", "wait": 10}
			if err := os.WriteFile(marker, []byte("disposable"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := exec.Execute(t.Context(), args)
			if err != nil || result.IsError {
				t.Fatalf("approved command: %+v, %v", result, err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("approved command did not remove fixture: %v", err)
			}
			if err := os.WriteFile(marker, []byte("new user work"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := exec.Execute(t.Context(), args); err == nil || calls != 2 {
				t.Fatalf("reused approval: calls=%d, err=%v", calls, err)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "new user work" {
				t.Fatalf("denied repeat changed user work: %q, %v", data, err)
			}
		})
	}
}

func TestExecChecksLifecycleBeforeStarting(t *testing.T) {
	for _, scenario := range []string{"cancelled", "closed", "full"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
			m := NewExecManager(nil)
			defer m.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := ""
			switch scenario {
			case "cancelled":
				cancel()
				want = "context canceled"
			case "closed":
				m.Close()
				want = "agent session is closed"
			case "full":
				for range maxExecSessions {
					if _, err := m.add(&execSession{done: make(chan struct{}), cancel: func() {}}); err != nil {
						t.Fatal(err)
					}
				}
				want = "too many running sessions"
			}
			_, err := ExecTools(m, t.TempDir(), nil, &Options{DisableSandbox: true})[0].Execute(ctx, map[string]any{"command": "echo side-effect", "wait": 0})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("expected %q before trying to start process, got %v", want, err)
			}
		})
	}
}

func TestExecRejectsInvalidOptionsBeforeStarting(t *testing.T) {
	for key, value := range map[string]any{"timeout": int64(math.MaxInt64), "tty": "true", "validation": 1, "description": true, "workdir": 1} {
		t.Run(key, func(t *testing.T) {
			m := NewExecManager(nil)
			defer m.Close()
			_, err := ExecTools(m, t.TempDir(), nil, &Options{DisableSandbox: true})[0].Execute(t.Context(), map[string]any{"command": "true", key: value})
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("invalid %s accepted: %v", key, err)
			}
		})
	}
}

func TestExecSessionCancelledInputHasNoSideEffects(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()
	writer := &reviewWriteCloser{}
	s := &execSession{stdin: writer, done: make(chan struct{}), cancel: func() {}}
	id, err := m.add(s)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = executeExecSession(ctx, m, nil, map[string]any{"session_id": id, "input": "hello", "wait": 0})
	if !errors.Is(err, context.Canceled) || writer.Len() != 0 {
		t.Fatalf("cancelled input was written: %q, %v", writer.String(), err)
	}
}

func TestExecKillReportsValidationExitStatus(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()
	tools := ExecTools(m, t.TempDir(), nil, &Options{DisableSandbox: true})
	started, err := tools[0].Execute(t.Context(), map[string]any{"command": "sleep 30", "validation": true, "wait": 0})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools[1].Execute(t.Context(), map[string]any{"session_id": started.Metadata["session_id"], "kill": true})
	if err != nil || !result.IsError || result.Metadata["exit_code"] != -1 || result.Metadata["duration_ms"] == nil {
		t.Fatalf("killed validation lost exit status: %+v, %v", result, err)
	}
}

func TestExecBlockedPipeWriteTerminatesOnCancellation(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()
	tools := ExecTools(m, t.TempDir(), nil, &Options{DisableSandbox: true})
	started, err := tools[0].Execute(t.Context(), map[string]any{"command": "sleep 30", "wait": 0})
	if err != nil {
		t.Fatal(err)
	}
	id := started.Metadata["session_id"].(int)
	s := m.get(id)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = tools[1].Execute(ctx, map[string]any{"session_id": id, "input": strings.Repeat("x", 1024*1024), "wait": 0})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked write = %v", err)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled input left process running")
	}
}

func TestExecFailedStartReleasesCapacity(t *testing.T) {
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	m := NewExecManager(nil)
	defer m.Close()
	tools := ExecTools(m, t.TempDir(), nil, &Options{DisableSandbox: true})
	for range maxExecSessions + 1 {
		if _, err := tools[0].Execute(t.Context(), map[string]any{"command": "true"}); err == nil || !strings.Contains(err.Error(), "failed to start") {
			t.Fatalf("failed launch leaked capacity: %v", err)
		}
	}
	if len(m.sessions) != 0 {
		t.Fatalf("failed launches left %d sessions", len(m.sessions))
	}
}
