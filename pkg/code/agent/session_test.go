package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

func TestCloseReportsAllSessionFailuresAndStillRunsEndHooks(t *testing.T) {
	a := New(newOptionsTestWorkspace(t), Config{Options: Options{DisableShell: true}}, nil)
	defer a.Close()
	failures := []error{errors.New("first cleanup failed"), errors.New("second cleanup failed")}
	var ended int
	var ids []string
	for _, failure := range failures {
		id, err := a.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		s := a.session(id)
		s.toolSet.Own("failing resource", func() error { return failure })
		s.aa.Hooks.SessionEnd = append(s.aa.Hooks.SessionEnd, func(context.Context, string) { ended++ })
	}
	err := a.Close()
	for i, failure := range failures {
		if !errors.Is(err, failure) || !strings.Contains(err.Error(), ids[i]) {
			t.Fatalf("close lost session %s's error: %v", ids[i], err)
		}
	}
	if ended != 2 {
		t.Fatalf("ran %d end hooks, want 2", ended)
	}
	if err := a.Close(); err != nil || ended != 2 {
		t.Fatalf("repeated close = %v, end hooks = %d", err, ended)
	}
}

func TestDeleteSessionReportsCleanupFailureAndCompletesDeletion(t *testing.T) {
	failure := errors.New("host cleanup failed")
	a := New(newOptionsTestWorkspace(t), Config{
		Options: Options{DisableShell: true},
		Config:  harness.Config{CloseSession: func() error { return failure }},
	}, nil)
	defer a.Close()
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.session(id).aa.RecordMessage(harness.Message{Role: harness.RoleUser, Content: []harness.Content{{Text: "saved"}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(id); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteSession(t.Context(), id); !errors.Is(err, failure) {
		t.Fatalf("delete error = %v", err)
	}
	if a.HasSession(id) {
		t.Fatal("deleted session is still registered")
	}
	if _, err := session.Load(a.sessionsDir, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted session can still be loaded: %v", err)
	}
}

func TestRegistrationFailurePreservesCancellationAndCleanupError(t *testing.T) {
	failure := errors.New("host cleanup failed")
	var ended int
	a := New(newOptionsTestWorkspace(t), Config{
		Options: Options{DisableShell: true},
		Config: harness.Config{
			CloseSession: func() error { return failure },
			Hooks:        hook.Hooks{SessionEnd: []hook.SessionEnd{func(context.Context, string) { ended++ }}},
		},
	}, nil)
	defer a.Close()
	s, err := a.buildSession("canceled-registration")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = a.registerSession(ctx, s)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, failure) || ended != 1 || a.HasSession(s.id) {
		t.Fatalf("register error = %v, end hooks = %d, registered = %t", err, ended, a.HasSession(s.id))
	}
}

func TestFailedSessionConstructionRemovesPrivateTemp(t *testing.T) {
	a := New(newOptionsTestWorkspace(t), Config{Options: Options{DisableShell: true}}, nil)
	defer a.Close()
	artifacts, err := session.ArtifactDir(a.sessionsDir, "invalid-tasks")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(artifacts, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "tasks.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, temp)
	}
	if _, err := a.buildSession("invalid-tasks"); err == nil {
		t.Fatal("invalid task registry was accepted")
	}
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed construction left temporary resources: %v, %v", entries, err)
	}
}

func TestRestoredSubagentInheritsHooksBeforeToolConstruction(t *testing.T) {
	a, id, _ := cancellationE2EAgent(t, hook.Hooks{})
	if err := a.session(id).aa.RecordMessage(harness.Message{Role: harness.RoleUser, Content: []harness.Content{{Text: "saved"}}}); err != nil {
		t.Fatal(err)
	}
	const agentID = "restored-child"
	spec := json.RawMessage(`{"version":1,"agent_id":"restored-child","agent_type":"explore","instructions":"answer briefly","model":"test-model"}`)
	created, err := a.session(id).tasks.AdoptAgent(agentID, "restored", "explore", "done", time.Second, func(tk *task.Task) error {
		return tk.SetDurableAgent(agentID, harness.State{}, spec)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Save(id); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	path := userHooksConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"http","url":%q}]}]}}`, server.URL)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	restored := New(a.workspace, a.cfg, nil)
	defer restored.Close()
	if err := restored.LoadSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	registry := restored.session(id).tasks
	child := registry.Get(created.ID)
	if child == nil || !child.SupportsResume() {
		t.Fatal("restored child cannot resume")
	}
	if err := child.ResumeContext(t.Context(), "continue"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-registry.Events():
		if event.ID != child.ID || event.Seq != 2 || event.Status != task.StatusDone {
			t.Fatalf("restored child failed: %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restored child did not finish")
	}
	if calls.Load() != 1 {
		t.Fatalf("restored child ran %d Stop hooks, want 1", calls.Load())
	}
}
