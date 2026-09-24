package task_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/task"
)

func TestFileRegistryRetriesCompletionWithoutRerunningTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tasks.json")
		r, err := task.NewFileRegistry(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		release := make(chan struct{})
		calls := 0
		tk, err := r.Launch("inspect", "explore", func(context.Context, *task.Task) (string, error) {
			calls++
			<-release
			return "finished report", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		restoreWrites := blockRegistryWrites(t, path)
		close(release)
		synctest.Wait() // The first save failed; execute is waiting to retry.
		if tk.Status() != task.StatusRunning || calls != 1 {
			t.Fatalf("completion escaped before save: status=%s calls=%d", tk.Status(), calls)
		}
		select {
		case event := <-r.Events():
			t.Fatalf("premature completion: %+v", event)
		default:
		}
		if err := r.Relaunch(tk, func(context.Context, *task.Task) (string, error) {
			t.Error("task relaunched while its result was being saved")
			return "", nil
		}); err == nil {
			t.Fatal("relaunch succeeded during completion save")
		}
		restoreWrites()
		event := waitEvent(t, r)
		if event.Status != task.StatusDone || event.Result != "finished report" || calls != 1 {
			t.Fatalf("completion=%+v calls=%d", event, calls)
		}
		r.Close()
		assertRestoredTask(t, path, tk.ID, task.StatusDone, "finished report")
	})
}

func TestFileRegistryReportsUnsavedCompletionAndPreservesResult(t *testing.T) {
	for _, status := range []task.Status{task.StatusDone, task.StatusFailed, task.StatusStopped} {
		t.Run(string(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "tasks.json")
				r, err := task.NewFileRegistry(path)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				release := make(chan struct{})
				tk, err := r.Launch("inspect", "explore", func(context.Context, *task.Task) (string, error) {
					<-release
					if status == task.StatusFailed {
						return "", errors.New("execution failed")
					}
					return "finished report", nil
				})
				if err != nil {
					t.Fatal(err)
				}
				restoreWrites := blockRegistryWrites(t, path)
				if status == task.StatusStopped {
					if err := r.Stop(tk.ID); err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				event := waitEvent(t, r)
				wantResult := "finished report"
				if status == task.StatusFailed {
					wantResult = "error: execution failed"
				}
				if event.Status != task.StatusFailed || tk.Status() != task.StatusFailed || event.Result != tk.Result() {
					t.Fatalf("save failure not exposed: %+v", event)
				}
				if !strings.Contains(event.Notice(), "failed") || !strings.Contains(event.Notification(), "could not save") || !strings.Contains(event.Notification(), "Execution status: "+string(status)) || !strings.HasSuffix(event.Result, wantResult) {
					t.Fatalf("notification must explain the save failure and preserve the execution outcome: %s", event.Notification())
				}
				restoreWrites()
				// A later successful registry write saves prior results too.
				if r.Adopt("next task", "explore", "next report", 0) == nil {
					t.Fatal("save after filesystem recovery failed")
				}
				r.Close()
				assertRestoredTask(t, path, tk.ID, task.StatusFailed, event.Result)
			})
		})
	}
}

func TestFileRegistryCloseCancelsCompletionRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tasks.json")
		r, err := task.NewFileRegistry(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		release := make(chan struct{})
		tk, err := r.Launch("inspect", "explore", func(context.Context, *task.Task) (string, error) {
			<-release
			return "finished report", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		blockRegistryWrites(t, path)
		close(release)
		synctest.Wait()
		r.Close()
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if tk.Status() != task.StatusDone {
			t.Fatal("completion retry did not stop on close")
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("closed registry recreated its directory: %v", err)
		}
		select {
		case event := <-r.Events():
			t.Fatalf("completion delivered after close: %+v", event)
		default:
		}
	})
}

// A directory at the destination makes atomic rename fail on all platforms,
// unlike file permission changes that privileged test processes can bypass.
func blockRegistryWrites(t *testing.T, path string) func() {
	t.Helper()
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, path); err != nil {
			t.Fatal(err)
		}
	}
}

func assertRestoredTask(t *testing.T, path, id string, status task.Status, result string) {
	t.Helper()
	restored, err := task.NewFileRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	tk := restored.Get(id)
	if tk == nil || tk.Status() != status || tk.Result() != result {
		t.Fatalf("restored task=%+v", tk)
	}
}

func durableState(text string) agent.State {
	message := agent.Message{Role: agent.RoleUser, Content: []agent.Content{{Text: text}}}
	return agent.State{Events: []agent.RuntimeEvent{{
		Sequence: 1, ID: "message-1", Type: agent.EventMessage, At: time.Now().UTC(), Message: &message,
	}}}
}

func TestFileRegistryRestoresCompletedTaskAndChildLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	r, err := task.NewFileRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"version":1}`)
	launched, err := r.LaunchAgent("agent-identity-123", "inspect", "explore", func(tk *task.Task) error {
		return tk.SetDurableAgent("agent-identity-123", durableState("child context"), raw)
	}, func(context.Context, *task.Task) (string, error) {
		return "finished report", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if event := waitEvent(t, r); event.Status != task.StatusDone {
		t.Fatalf("completion = %#v", event)
	}
	r.Close()

	restored, err := task.NewFileRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got := restored.Get(launched.ID)
	if got == nil || got.Status() != task.StatusDone || got.Result() != "finished report" {
		t.Fatalf("restored task = %#v", got)
	}
	agentID, state, resume := got.DurableAgent()
	if agentID != "agent-identity-123" || string(resume) != string(raw) {
		t.Fatalf("identity=%q resume=%s", agentID, resume)
	}
	if len(state.Events) != 1 || len(got.PeekMessages()) != 1 || got.PeekMessages()[0].Content[0].Text != "child context" {
		t.Fatalf("restored child state = %#v", state)
	}
}

func TestFileRegistryReconcilesRunningTaskWithoutReplaying(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	r, err := task.NewFileRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	launched, err := r.LaunchAgent("agent-running-123", "inspect", "explore", func(tk *task.Task) error {
		return tk.SetDurableAgent("agent-running-123", durableState("partial context"), json.RawMessage(`{"version":1}`))
	}, func(context.Context, *task.Task) (string, error) {
		<-release
		return "should not be replayed", nil
	})
	if err != nil {
		t.Fatal(err)
	}

	restored, err := task.NewFileRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got := restored.Get(launched.ID)
	if got == nil || got.Status() != task.StatusFailed {
		t.Fatalf("restored running task = %#v", got)
	}
	select {
	case event := <-restored.Events():
		if event.ID != launched.ID || event.Status != task.StatusFailed {
			t.Fatalf("reconciliation event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing interrupted-task notification")
	}
	if len(got.PeekMessages()) != 1 {
		t.Fatal("partial child context was lost")
	}

	restored.Close()
	close(release)
	_ = waitEvent(t, r)
	r.Close()
}
