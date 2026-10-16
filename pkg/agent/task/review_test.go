package task_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/task"
)

func TestReviewFailedTaskPreservesIncompleteOutput(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()
	if _, err := r.Launch("work", "explore", func(context.Context, *task.Task) (string, error) { return "partial findings", errors.New("failed") }); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, r)
	if ev.Status != task.StatusFailed || !strings.Contains(ev.Result, "partial findings") || !strings.Contains(ev.Result, "incomplete") {
		t.Fatalf("partial output lost or unmarked: %+v", ev)
	}
}

func TestReviewRelaunchClearsPreviousActivity(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()
	tk := r.Adopt("work", "explore", "done", 0)
	before := tk.Info()
	tk.SetActivity("previous command")
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	if err := r.Relaunch(tk, func(context.Context, *task.Task) (string, error) { close(started); <-release; return "new report", nil }); err != nil {
		t.Fatal(err)
	}
	<-started
	if tk.Activity() != "" {
		t.Fatalf("follow-up displays previous activity: %q", tk.Activity())
	}
	info := tk.Info()
	if info.Status != task.StatusRunning || info.Seq != 2 || info.Result != "" || info.Activity != "" {
		t.Fatalf("mixed task state after relaunch: %+v", info)
	}
	if before.Status != task.StatusDone || before.Seq != 1 || before.Result != "done" {
		t.Fatalf("earlier snapshot changed: %+v", before)
	}
}

func TestReviewRelaunchPersistFailureRollsBackTaskState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	r, err := task.NewFileRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tk := r.Adopt("work", "explore", "original result", 0)
	if tk == nil {
		t.Fatal("Adopt returned nil")
	}
	restoreWrites := blockRegistryWrites(t, path)
	defer restoreWrites()
	if err := r.Relaunch(tk, func(context.Context, *task.Task) (string, error) {
		t.Error("relaunch ran after persist failure")
		return "new result", nil
	}); err == nil {
		t.Fatal("expected relaunch persist error")
	}
	info := tk.Info()
	if info.Status != task.StatusDone || info.Result != "original result" || info.Seq != 1 {
		t.Fatalf("failed relaunch clobbered task state: %+v", info)
	}
}

