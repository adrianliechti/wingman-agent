package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
)

func TestScheduleRunnerDeliversSessionNotification(t *testing.T) {
	s := &sessionState{
		id:        "scheduled-session",
		parent:    &Agent{workspace: &code.Workspace{RootPath: t.TempDir()}},
		resources: sessionResources{tasks: task.NewRegistry()},
		schedules: schedule.NewMemoryStore(),
	}
	defer s.resources.tasks.Close()
	if err := s.schedules.Mutate(func(tasks []schedule.Task) ([]schedule.Task, error) {
		return append(tasks, schedule.Task{ID: "job", Prompt: "check deploy", Schedule: "every 1h", Script: "echo build-broken", Status: schedule.StatusActive, CreatedAt: time.Now().Add(-2 * time.Hour)}), nil
	}); err != nil {
		t.Fatal(err)
	}
	s.scheduleRunner().FireDue(context.Background())
	select {
	case ev := <-s.resources.tasks.Events():
		if ev.Kind != task.KindSchedule || ev.ID != "job" || ev.Seq != 1 || ev.Description != "every 1h" {
			t.Fatalf("event = %+v", ev)
		}
		if note := ev.Notification(); !strings.Contains(note, "<scheduled-task>") || !strings.Contains(note, "check deploy") || !strings.Contains(note, "build-broken") {
			t.Fatalf("notification = %q", note)
		}
	case <-time.After(time.Second):
		t.Fatal("schedule notification was not delivered")
	}
}
