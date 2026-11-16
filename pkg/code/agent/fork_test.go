package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

func seedForkSource(t *testing.T, a *Agent, id string) {
	t.Helper()
	state := harness.State{Messages: []harness.Message{
		{Role: harness.RoleUser, Content: []harness.Content{{Text: "hi"}}},
		{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "hello"}}},
	}}
	if err := session.Save(a.sessionsDir, id, state); err != nil {
		t.Fatal(err)
	}
}

func TestForkSessionBeforeFirstPrompt(t *testing.T) {
	a := New(newOptionsTestWorkspace(t), Config{}, nil)
	defer a.Close()
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetModel(t.Context(), id, "gpt-6-sol"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMode(t.Context(), id, code.PlanModeID); err != nil {
		t.Fatal(err)
	}
	fork, err := a.ForkSession(t.Context(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	if fork == id || !a.HasSession(id) || !a.HasSession(fork) || len(a.Messages(fork)) != 0 {
		t.Fatalf("invalid empty fork %q from %q", fork, id)
	}
	if _, model := a.Models(fork); model != "gpt-6-sol" {
		t.Fatalf("fork model = %q", model)
	}
	if _, mode := a.Modes(fork); mode != code.PlanModeID {
		t.Fatalf("fork mode = %q", mode)
	}
}

func TestForkSessionCarriesHistorySettingsAndMode(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	a := New(ws, Config{Options: Options{IsolateSessions: true}}, nil)
	defer a.Close()

	const id = "fork-source"
	seedForkSource(t, a, id)
	if err := a.LoadSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := a.SetModel(t.Context(), id, "gpt-6-sol"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetEffort(t.Context(), id, "high"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMode(t.Context(), id, code.PlanModeID); err != nil {
		t.Fatal(err)
	}
	if err := a.SetUnattended(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	err := a.Schedules(id).Mutate(func(tasks []schedule.Task) ([]schedule.Task, error) {
		return append(tasks, schedule.Task{ID: "job-1", Prompt: "check", Schedule: "every 1h", Status: schedule.StatusActive, CreatedAt: time.Now()}), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	forkID, err := a.ForkSession(t.Context(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	if forkID == id || !a.HasSession(forkID) {
		t.Fatalf("fork id = %q, loaded = %v", forkID, a.HasSession(forkID))
	}
	if got, want := len(a.Messages(forkID)), len(a.Messages(id)); got != want || got != 2 {
		t.Fatalf("fork messages = %d, source = %d", got, want)
	}
	if _, current := a.Models(forkID); current != "gpt-6-sol" {
		t.Fatalf("fork model = %q", current)
	}
	if effort, _ := a.Effort(forkID); effort != "high" {
		t.Fatalf("fork effort = %q", effort)
	}
	if _, mode := a.Modes(forkID); mode != code.PlanModeID || !a.Unattended(forkID) {
		t.Fatalf("fork mode = %q unattended = %v", mode, a.Unattended(forkID))
	}
	if tasks, _ := a.Schedules(forkID).List(); len(tasks) != 0 {
		t.Fatalf("fork schedules = %+v, want none", tasks)
	}
	if tasks, _ := a.Schedules(id).List(); len(tasks) != 1 {
		t.Fatalf("source schedules = %+v, want the original", tasks)
	}

	sessions, err := a.ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var title string
	for _, info := range sessions {
		if info.ID == forkID {
			title = info.Title
		}
	}
	if title != "Fork of hi" {
		t.Fatalf("fork title = %q", title)
	}
}

func TestForkSessionFromUnloadedSession(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	a := New(ws, Config{Options: optionsFromEnvironment()}, nil)
	defer a.Close()

	const id = "on-disk"
	seedForkSource(t, a, id)

	forkID, err := a.ForkSession(t.Context(), id, "Second try")
	if err != nil {
		t.Fatal(err)
	}
	if a.HasSession(id) {
		t.Fatal("forking loaded the source session")
	}
	if len(a.Messages(forkID)) != 2 {
		t.Fatalf("fork messages = %d", len(a.Messages(forkID)))
	}
	if s, err := session.Load(a.sessionsDir, forkID); err != nil || s.Title != "Second try" {
		t.Fatalf("fork title = %q, %v", s.Title, err)
	}
}

func TestForkSessionRefusesRunningTurn(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	a := New(ws, Config{Options: optionsFromEnvironment()}, nil)
	defer a.Close()

	const id = "busy"
	seedForkSource(t, a, id)
	if err := a.LoadSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	source := a.session(id)
	busy, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = source.harness.RunExclusive(t.Context(), func(context.Context) error {
			close(busy)
			<-release
			return nil
		})
	}()
	<-busy
	defer func() { close(release); <-done }()

	if _, err := a.ForkSession(t.Context(), id, ""); !errors.Is(err, code.ErrTurnInProgress) {
		t.Fatalf("fork while busy = %v, want ErrTurnInProgress", err)
	}
	entries, err := os.ReadDir(a.sessionsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".jsonl" && entry.Name() != id+".jsonl" {
			t.Fatalf("busy fork left %s behind", entry.Name())
		}
	}
}

func TestTruncateSessionRewindsALoadedSession(t *testing.T) {
	a := New(newOptionsTestWorkspace(t), Config{Options: Options{IsolateSessions: true}}, nil)
	defer a.Close()
	const id = "truncate-source"
	var events []harness.RuntimeEvent
	add := func(e harness.RuntimeEvent) {
		e.Sequence = uint64(len(events) + 1)
		e.ID = "event-" + strconv.FormatUint(e.Sequence, 10)
		e.At = time.Now().UTC()
		events = append(events, e)
	}
	for _, input := range []string{"a", "b"} {
		add(harness.RuntimeEvent{Type: harness.EventTurnStarted, TurnID: "turn-" + input})
		add(harness.RuntimeEvent{Type: harness.EventMessage, Message: &harness.Message{Role: harness.RoleUser, InputID: input, Content: []harness.Content{{Text: "ask " + input}}}})
		add(harness.RuntimeEvent{Type: harness.EventMessage, Message: &harness.Message{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "answer " + input}}}})
		add(harness.RuntimeEvent{Type: harness.EventTurnTerminal, TurnID: "turn-" + input, Terminal: &harness.RuntimeTerminal{Status: harness.RuntimeCompleted}})
	}
	if err := session.Save(a.sessionsDir, id, harness.State{Events: events}); err != nil {
		t.Fatal(err)
	}
	if err := a.LoadSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMode(t.Context(), id, code.PlanModeID); err != nil {
		t.Fatal(err)
	}
	version := a.HistoryVersion(id)
	if err := a.TruncateSession(t.Context(), id, "missing"); err == nil || len(a.Messages(id)) != 4 {
		t.Fatalf("cut after an unknown input: %v, %d messages", err, len(a.Messages(id)))
	}
	if err := a.TruncateSession(t.Context(), id, "a"); err != nil {
		t.Fatal(err)
	}
	messages := a.Messages(id)
	if len(messages) != 2 || messages[1].Content[0].Text != "answer a" {
		t.Fatalf("messages after the cut = %+v", messages)
	}
	// The rewrite is visible as one, and the session keeps its settings.
	if _, mode := a.Modes(id); a.HistoryVersion(id).Revision == version.Revision || mode != code.PlanModeID {
		t.Fatalf("revision %d, mode %q", a.HistoryVersion(id).Revision, mode)
	}
	if saved, err := session.Load(a.sessionsDir, id); err != nil || len(saved.State.Messages) != 2 {
		t.Fatalf("saved after the cut = %+v, %v", saved.State.Messages, err)
	}
	if err := a.TruncateSession(t.Context(), id, ""); err != nil || len(a.Messages(id)) != 0 {
		t.Fatalf("clearing: %v, %d messages", err, len(a.Messages(id)))
	}
}
