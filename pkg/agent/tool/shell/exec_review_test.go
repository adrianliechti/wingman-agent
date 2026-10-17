package shell

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestExecSessionPollReturnsWhenOutputArrives(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()
	s := &execSession{done: make(chan struct{}), cancel: func() {}}
	id, err := m.add(s)
	if err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(50*time.Millisecond, func() { s.Write([]byte("ready")) })
	defer timer.Stop()
	started := time.Now()
	result, err := executeExecSession(t.Context(), m, nil, map[string]any{"session_id": id, "wait": 2})
	if err != nil || !strings.Contains(result.Content, "ready") {
		t.Fatalf("poll = %+v, %v", result, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("poll waited after output became available")
	}
	started = time.Now()
	result, err = executeExecSession(t.Context(), m, nil, map[string]any{"session_id": id, "wait": 1})
	if err != nil || !strings.Contains(result.Content, "no new output") || time.Since(started) < 500*time.Millisecond {
		t.Fatalf("stale output woke a later poll: %+v, %v", result, err)
	}
}

func TestExecKillKeepsSessionUntilExit(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()
	s := &execSession{done: make(chan struct{}), cancel: func() {}}
	id, err := m.add(s)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	result, err := executeExecSession(ctx, m, nil, map[string]any{"session_id": id, "kill": true})
	if err != nil || !strings.Contains(result.Content, "has not exited yet") {
		t.Fatalf("kill = %+v, %v", result, err)
	}
	if m.get(id) != s {
		t.Fatal("kill discarded a still-running session")
	}
}

func TestExecBufferBoundsAllocationAndKeepsUTF8AcrossPolls(t *testing.T) {
	s := &execSession{done: make(chan struct{})}
	s.Write(bytes.Repeat([]byte("x"), 8*maxUnreadBytes))
	if s.unread.Cap() > 2*maxUnreadBytes {
		t.Fatalf("buffer allocated %d bytes for a %d-byte limit", s.unread.Cap(), maxUnreadBytes)
	}
	s.drain()
	s.Write([]byte("hello\xc3"))
	if got := s.drain(); got != "hello" {
		t.Fatalf("incomplete character was emitted: %q", got)
	}
	s.Write([]byte("\xa9"))
	if got := s.drain(); got != "é" {
		t.Fatalf("split character was lost: %q", got)
	}
	s.Write([]byte(strings.Repeat("€", maxUnreadBytes/3+1)))
	if got := s.drain(); !utf8.ValidString(got) {
		t.Fatal("buffer truncation emitted invalid UTF-8")
	}
	s.Write([]byte{0xff})
	close(s.done)
	if got := s.drain(); !utf8.ValidString(got) || got == "" {
		t.Fatalf("invalid process output was not made displayable: %q", got)
	}
}

func TestExecKeepsTerminalEscapesAcrossPolls(t *testing.T) {
	for _, sequence := range []string{"\x1b[31m", "\x1b]0;title\x07", "\x1b]0;title\x1b\\"} {
		for split := 1; split < len(sequence); split++ {
			s := &execSession{done: make(chan struct{})}
			s.Write([]byte("before" + sequence[:split]))
			if got := s.drain(); got != "before" {
				t.Fatalf("first half of %q split at %d = %q", sequence, split, got)
			}
			s.Write([]byte(sequence[split:] + "after"))
			if got := s.drain(); got != "after" {
				t.Fatalf("second half of %q split at %d = %q", sequence, split, got)
			}
		}
	}
}

func TestExecSessionInputLockCanBeCancelled(t *testing.T) {
	s := &execSession{}
	if err := s.lockInput(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.unlockInput()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := s.lockInput(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("input lock ignored cancellation: %v", err)
	}
}

func TestExecSessionRejectsMalformedOptionsWithoutWriting(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()
	writer := &reviewWriteCloser{}
	s := &execSession{stdin: writer, done: make(chan struct{}), cancel: func() {}}
	id, err := m.add(s)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]any{"input": 3, "kill": "false", "eof": "true", "wait": 0.5, "session_id": 0} {
		args := map[string]any{"session_id": id, "input": "hello", "wait": 0}
		args[key] = value
		if _, err := executeExecSession(t.Context(), m, nil, args); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("invalid %s accepted: %v", key, err)
		}
	}
	if writer.Len() != 0 {
		t.Fatalf("invalid call wrote input: %q", writer.String())
	}
}

func TestExecSessionApprovalDoesNotCarryAcrossInteractiveState(t *testing.T) {
	calls := 0
	elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) {
		calls++
		return true, nil
	}}
	m := NewExecManager(nil)
	defer m.Close()
	s := &execSession{stdin: &reviewWriteCloser{}, done: make(chan struct{})}
	for range 2 {
		if _, err := writeSessionInput(t.Context(), m, 1, s, elicit, "rm -rf relative-dir\n"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("reused approval despite mutable interactive cwd/state: %d prompts", calls)
	}
}

func TestExecSessionDeniedSplitCommandIsNotSubmitted(t *testing.T) {
	for _, eof := range []bool{false, true} {
		m := NewExecManager(nil)
		writer := &reviewWriteCloser{}
		s := &execSession{stdin: writer, done: make(chan struct{})}
		calls := 0
		elicit := &tool.Elicitation{Review: func(_ context.Context, approval tool.Approval) (tool.Verdict, error) {
			calls++
			if !strings.Contains(approval.Message, "rm --rec fixture") {
				t.Fatalf("review lost earlier input: %s", approval.Message)
			}
			return tool.Verdict{Reason: "fixture denial"}, nil
		}}
		if _, err := writeSessionInput(t.Context(), m, 1, s, elicit, "rm --rec fixture"); err != nil {
			t.Fatal(err)
		}
		var err error
		if eof {
			_, err = applySessionInput(t.Context(), m, s, elicit, "", true)
		} else {
			_, err = writeSessionInput(t.Context(), m, 1, s, elicit, "\n")
		}
		if err == nil || calls != 1 || writer.String() != "rm --rec fixture" {
			t.Fatalf("eof=%t: denied command submitted: %q, reviews=%d, err=%v", eof, writer.String(), calls, err)
		}
		m.Close()
	}
}

type reviewWriteCloser struct{ bytes.Buffer }

func (*reviewWriteCloser) Close() error { return nil }

func TestExecSessionBlockedInputCanBeCancelled(t *testing.T) {
	m := NewExecManager(nil)
	defer m.Close()
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	killed := make(chan struct{}, 1)
	s := &execSession{stdin: w, done: make(chan struct{}), cancel: func() {
		select {
		case killed <- struct{}{}:
		default:
		}
	}}
	id, err := m.add(s)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := executeExecSession(ctx, m, nil, map[string]any{"session_id": id, "input": "hello", "wait": 0})
		finished <- err
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancelled input returned %v", err)
		}
	case <-time.After(time.Second):
		w.Close()
		<-finished
		t.Fatal("stdin write ignored cancellation")
	}
	select {
	case <-killed:
	default:
		t.Fatal("partially submitted input was left running after cancellation")
	}
}

func TestExecSessionOverflowPersistsAcrossBackspaceOnTTY(t *testing.T) {
	calls := 0
	var message string
	elicit := &tool.Elicitation{Confirm: func(_ context.Context, prompt string) (bool, error) {
		calls++
		message = prompt
		return false, nil
	}}
	m := NewExecManager(nil)
	defer m.Close()
	s := &execSession{tty: true, stdin: &reviewWriteCloser{}, done: make(chan struct{})}

	longPrefix := strings.Repeat("a", maxClassifiableBytes) + "; rm -rf /"
	if _, err := writeSessionInput(t.Context(), m, 1, s, elicit, longPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSessionInput(t.Context(), m, 1, s, elicit, "\b"); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSessionInput(t.Context(), m, 1, s, elicit, "\n"); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("overflowed line after backspace was not gated: %v", err)
	}
	if calls != 1 || !strings.Contains(message, "exceeded classifier limit") {
		t.Fatalf("approval = %q (%d calls)", message, calls)
	}
}

func TestClassifyExecSessionFlagsControlInput(t *testing.T) {
	if got := classifyExecSession(map[string]any{"session_id": 1, "input": `\t`}); got != tool.EffectDangerous {
		t.Fatalf("tab completion effect = %v, want EffectDangerous", got)
	}
	if got := classifyExecSession(map[string]any{"session_id": 1, "input": `\u0003`}); got != tool.EffectMutates {
		t.Fatalf("ctrl-c effect = %v, want EffectMutates", got)
	}
}
