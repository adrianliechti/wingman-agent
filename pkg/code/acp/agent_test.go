package acp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestToolCallContentTextRendersDiff(t *testing.T) {
	old := "line one\nold line\nline three\n"
	items := []acp.ToolCallContent{
		{Diff: &acp.ToolCallContentDiff{
			Type:    "diff",
			Path:    "/p/a.go",
			OldText: &old,
			NewText: "line one\nnew line\nline three\n",
		}},
	}
	got := toolCallContentText(items)
	if !strings.Contains(got, "/p/a.go") {
		t.Errorf("expected path in output:\n%s", got)
	}
	if !strings.Contains(got, "-old line") || !strings.Contains(got, "+new line") {
		t.Errorf("expected -/+ diff lines:\n%s", got)
	}
	if !strings.Contains(got, " line one") {
		t.Errorf("expected unchanged context line:\n%s", got)
	}
}

func TestToolCallContentTextAddedFile(t *testing.T) {

	items := []acp.ToolCallContent{
		{Diff: &acp.ToolCallContentDiff{
			Type:    "diff",
			Path:    "/p/new.go",
			NewText: "package main\n\nfunc main() {}\n",
		}},
	}
	got := toolCallContentText(items)
	if strings.Contains(got, "-") {
		t.Errorf("added file should have no removed lines:\n%s", got)
	}
	if !strings.Contains(got, "+package main") || !strings.Contains(got, "+func main() {}") {
		t.Errorf("expected all lines added:\n%s", got)
	}
}

func TestToolCallContentTextPlainText(t *testing.T) {
	items := []acp.ToolCallContent{
		{Content: &acp.ToolCallContentContent{Content: acp.TextBlock("hello output")}},
	}
	if got := toolCallContentText(items); got != "hello output" {
		t.Errorf("plain text = %q", got)
	}
}

func modeState(modes ...string) *acp.SessionModeState {
	avail := make([]acp.SessionMode, 0, len(modes))
	for _, m := range modes {
		avail = append(avail, acp.SessionMode{Id: acp.SessionModeId(m), Name: m})
	}
	return &acp.SessionModeState{
		AvailableModes: avail,
		CurrentModeId:  acp.SessionModeId(modes[0]),
	}
}

func TestModesPerSession(t *testing.T) {
	a := &Agent{sessions: map[string]*sessionState{}}
	add := func(id string, modes ...string) {
		s := &sessionState{id: acp.SessionId(id)}
		s.applyModes(modeState(modes...))
		a.sessions[id] = s
	}
	add("a", "plan", "code")
	add("b", "code", "plan")

	if modes, cur := a.Modes("a"); cur != "plan" || len(modes) != 2 {
		t.Fatalf("session a = (%v, %q), want 2 modes current plan", modes, cur)
	}
	if _, cur := a.Modes("b"); cur != "code" {
		t.Fatalf("session b current = %q, want code", cur)
	}
	if modes, cur := a.Modes("missing"); modes != nil || cur != "" {
		t.Fatalf("unknown session = (%v, %q), want (nil, \"\")", modes, cur)
	}
}

func TestTranslateUpdateSuppressesPromptUserEcho(t *testing.T) {
	a := &Agent{}
	sess := &sessionState{}
	turn := &turn{ignoreUserUpdates: true}

	if msg, ok := a.translateUpdate(sess, turn, acp.UpdateUserMessageText("echo")); ok {
		t.Fatalf("prompt user echo was emitted: %+v", msg)
	}
	if len(turn.emitted) != 0 {
		t.Fatalf("prompt user echo was persisted: %+v", turn.emitted)
	}

	turn.ignoreUserUpdates = false
	if _, ok := a.translateUpdate(sess, turn, acp.UpdateUserMessageText("history")); !ok {
		t.Fatal("load-session user message was suppressed")
	}
}

func TestTranslateUpdateReleasesCompletedToolCall(t *testing.T) {
	a := &Agent{}
	sess := &sessionState{toolCalls: map[string]toolCall{}}
	turn := &turn{}
	id := acp.ToolCallId("call-1")

	if _, ok := a.translateUpdate(sess, turn, acp.StartToolCall(id, "shell")); !ok {
		t.Fatal("tool call start was not translated")
	}
	if len(sess.toolCalls) != 1 {
		t.Fatalf("in-flight tool calls = %d, want 1", len(sess.toolCalls))
	}

	update := acp.UpdateToolCall(
		id,
		acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
		acp.WithUpdateContent([]acp.ToolCallContent{
			acp.ToolContent(acp.TextBlock("done")),
		}),
	)
	if _, ok := a.translateUpdate(sess, turn, update); !ok {
		t.Fatal("tool call completion was not translated")
	}
	if len(sess.toolCalls) != 0 {
		t.Fatalf("completed tool call was retained: %+v", sess.toolCalls)
	}
}

func steerTestAgent(fn func(context.Context, acp.SessionId, []acp.ContentBlock, string) error) (*Agent, *sessionState, *turn) {
	t := &turn{}
	sess := &sessionState{id: "session-1", inflight: t}
	a := &Agent{
		steer: fn,
		sessions: map[string]*sessionState{
			"session-1": sess,
		},
	}
	return a, sess, t
}

func TestSteerAcceptedBeforeFinalizationIsPersistedWithTurn(t *testing.T) {
	a, sess, active := steerTestAgent(func(context.Context, acp.SessionId, []acp.ContentBlock, string) error {
		return nil
	})
	input := code.TurnInput{ID: "input-1", Content: []agent.Content{{Text: "guide"}}}
	if err := a.Steer(context.Background(), "session-1", input); err != nil {
		t.Fatal(err)
	}
	sess.finalizeTurn(active)

	if len(sess.messages) != 1 || sess.messages[0].Role != agent.RoleUser || sess.messages[0].Content[0].Text != "guide" {
		t.Fatalf("messages = %+v", sess.messages)
	}
}

func TestSteerAcceptedAfterFinalizationIsStillPersisted(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	a, sess, active := steerTestAgent(func(context.Context, acp.SessionId, []acp.ContentBlock, string) error {
		close(started)
		<-release
		return nil
	})
	done := make(chan error, 1)
	go func() {
		done <- a.Steer(context.Background(), "session-1", code.TurnInput{
			ID: "input-1", Content: []agent.Content{{Text: "guide"}},
		})
	}()
	<-started
	sess.finalizeTurn(active)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if len(sess.messages) != 1 || sess.messages[0].Role != agent.RoleUser || sess.messages[0].Content[0].Text != "guide" {
		t.Fatalf("messages = %+v", sess.messages)
	}
}

func TestSteerFailureDoesNotPersistInput(t *testing.T) {
	want := errors.New("steer failed")
	a, sess, active := steerTestAgent(func(context.Context, acp.SessionId, []acp.ContentBlock, string) error {
		return want
	})
	err := a.Steer(context.Background(), "session-1", code.TurnInput{
		ID: "input-1", Content: []agent.Content{{Text: "guide"}},
	})
	if !errors.Is(err, want) {
		t.Fatalf("steer error = %v", err)
	}
	sess.finalizeTurn(active)
	if len(sess.messages) != 0 {
		t.Fatalf("failed steer was persisted: %+v", sess.messages)
	}
}

func TestSteerRequiresInflightTurn(t *testing.T) {
	a, sess, _ := steerTestAgent(func(context.Context, acp.SessionId, []acp.ContentBlock, string) error {
		return nil
	})
	sess.inflight = nil
	err := a.Steer(context.Background(), "session-1", code.TurnInput{ID: "input-1", Content: []agent.Content{{Text: "guide"}}})
	if !errors.Is(err, code.ErrNoActiveTurn) {
		t.Fatalf("steer error = %v", err)
	}
}
