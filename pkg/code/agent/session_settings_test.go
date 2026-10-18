package agent

import (
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

func TestLoadSessionRestoresSelectedModelAfterRestart(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	cfg := Config{Options: Options{IsolateSessions: true}}

	a := New(ws, cfg, nil)
	const id = "persisted-settings"
	if err := session.Save(a.sessionsDir, id, harness.State{Messages: []harness.Message{{Role: harness.RoleUser, Content: []harness.Content{{Text: "hi"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.LoadSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := a.SetModel(t.Context(), id, "gpt-6-sol"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetEffort(t.Context(), id, "high"); err != nil {
		t.Fatal(err)
	}
	a.Close()

	reopened := New(ws, cfg, nil)
	defer reopened.Close()
	// A model picked on the startup session must not leak into the resumed one.
	startup, err := reopened.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.SetModel(t.Context(), startup, "claude-fable-5"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.LoadSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, current := reopened.Models(id); current != "gpt-6-sol" {
		t.Fatalf("resumed model = %q, want gpt-6-sol", current)
	}
	if effort, _ := reopened.Effort(id); effort != "high" {
		t.Fatalf("resumed effort = %q, want high", effort)
	}
}

func TestRestoreSessionSettingsFallsBackToLastTurnModel(t *testing.T) {
	a := &Agent{sessionsDir: t.TempDir()}
	s := &sessionState{id: "legacy"}
	a.restoreSessionSettings(s, harness.State{Events: []harness.RuntimeEvent{
		{Type: harness.EventTurnStarted, Model: "gpt-6-astra"},
		{Type: harness.EventRunStarted, Model: "utility-model"},
		{Type: harness.EventTurnStarted, Model: "gpt-6-sol"},
		{Type: harness.EventRunStarted, Model: "utility-model"},
	}})
	if s.modelID != "gpt-6-sol" || s.effort != nil {
		t.Fatalf("restored model=%q effort=%v, want gpt-6-sol and inherited effort", s.modelID, s.effort)
	}
}
