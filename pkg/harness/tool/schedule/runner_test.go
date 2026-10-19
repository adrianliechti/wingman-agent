package schedule

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testRunner struct {
	*Runner
	events chan Fire
}

func newTestRunner(t *testing.T) *testRunner {
	t.Helper()
	root := t.TempDir()
	r := &testRunner{Runner: &Runner{Store: NewMemoryStore()}, events: make(chan Fire, 10)}
	r.Gate = func(ctx context.Context, task Task) (bool, string, error) {
		if task.Script == "" {
			return true, "", nil
		}
		return RunTaskGate(ctx, root, task, nil)
	}
	r.Deliver = func(_ context.Context, fire Fire) { r.events <- fire }
	return r
}

func seedTask(t *testing.T, s *testRunner, task Task) {
	t.Helper()

	err := s.Store.Mutate(func(tasks []Task) ([]Task, error) {
		return append(tasks, task), nil
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func waitEvent(t *testing.T, s *testRunner) (Fire, bool) {
	t.Helper()

	select {
	case ev := <-s.events:
		return ev, true
	case <-time.After(time.Second):
		return Fire{}, false
	}
}

func TestFireScheduleNotifiesSession(t *testing.T) {
	s := newTestRunner(t)

	seedTask(t, s, Task{
		ID:        "job-1",
		Prompt:    "check the deploy",
		Schedule:  "every 1h",
		Status:    StatusActive,
		CreatedAt: time.Now().Add(-2 * time.Hour),
	})

	tasks, _ := s.Store.List()
	s.fire(context.Background(), tasks[0])

	ev, ok := waitEvent(t, s)
	if !ok {
		t.Fatal("expected a schedule notification")
	}
	if ev.Task.ID != "job-1" {
		t.Fatalf("event = %+v, want a schedule event for job-1", ev)
	}
	if ev.Task.Schedule != "every 1h" || ev.Task.Prompt != "check the deploy" {
		t.Fatalf("event carries %q / %q, want the schedule and the prompt", ev.Task.Schedule, ev.Task.Prompt)
	}

	after, _ := s.Store.List()
	if len(after) != 1 || after[0].LastRun == nil {
		t.Fatalf("tasks = %#v, want the recurring task kept with a last run", after)
	}
	if after[0].Status != StatusActive {
		t.Fatalf("task status = %q, want it left active", after[0].Status)
	}
}

func TestFireScheduleDropsOneTimeTask(t *testing.T) {
	s := newTestRunner(t)

	at := time.Now().Add(-time.Minute)
	seedTask(t, s, Task{
		ID:        "once-1",
		Prompt:    "remind me",
		Schedule:  at.Format("2006-01-02T15:04:05"),
		Status:    StatusActive,
		CreatedAt: at.Add(-time.Hour),
	})

	tasks, _ := s.Store.List()
	s.fire(context.Background(), tasks[0])

	if _, ok := waitEvent(t, s); !ok {
		t.Fatal("expected a schedule notification")
	}

	after, _ := s.Store.List()
	if len(after) != 0 {
		t.Fatalf("tasks = %#v, want the one-time task removed", after)
	}
}

func TestFireScheduleHonorsPreCheckScript(t *testing.T) {
	s := newTestRunner(t)

	seedTask(t, s, Task{
		ID:        "gated",
		Prompt:    "only when changed",
		Schedule:  "every 1h",
		Script:    `echo '{"wake": false}'`,
		Status:    StatusActive,
		CreatedAt: time.Now().Add(-2 * time.Hour),
	})

	tasks, _ := s.Store.List()
	s.fire(context.Background(), tasks[0])

	select {
	case ev := <-s.events:
		t.Fatalf("wake=false should not notify, got %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}

	after, _ := s.Store.List()
	if after[0].LastRun == nil {
		t.Fatal("a skipped run still counts as a run for scheduling purposes")
	}
}

func TestFireScheduleAttachesGateOutput(t *testing.T) {
	s := newTestRunner(t)

	seedTask(t, s, Task{
		ID:        "gated-wake",
		Prompt:    "investigate",
		Schedule:  "every 1h",
		Script:    `echo build-broken`,
		Status:    StatusActive,
		CreatedAt: time.Now().Add(-2 * time.Hour),
	})

	tasks, _ := s.Store.List()
	s.fire(context.Background(), tasks[0])

	ev, ok := waitEvent(t, s)
	if !ok {
		t.Fatal("non-JSON gate output should fail open and notify")
	}
	if !strings.Contains(ev.Task.Prompt, "investigate") || !strings.Contains(ev.GateOutput, "build-broken") {
		t.Fatalf("result = %q, want the prompt plus the gate output", ev.GateOutput)
	}
}

func TestFireScheduleBacksOffFailingGate(t *testing.T) {
	s := newTestRunner(t)

	seedTask(t, s, Task{
		ID:        "broken",
		Prompt:    "poll",
		Schedule:  "every 15s",
		Script:    `exit 3`,
		Status:    StatusActive,
		CreatedAt: time.Now().Add(-time.Hour),
	})

	tasks, _ := s.Store.List()
	s.fire(context.Background(), tasks[0])

	ev, ok := waitEvent(t, s)
	if !ok {
		t.Fatal("a failing gate should fail open and notify")
	}
	if !strings.Contains(ev.GateOutput, "failed") {
		t.Fatalf("result = %q, want the failure note attached", ev.GateOutput)
	}

	after, _ := s.Store.List()
	if after[0].Failures != 1 || after[0].LastAttempt == nil {
		t.Fatalf("task = %+v, want one recorded failure", after[0])
	}
	if IsDue(after[0], time.Now().Add(time.Minute)) {
		t.Fatal("a failed task should back off past its own interval")
	}
	if !IsDue(after[0], time.Now().Add(3*time.Minute)) {
		t.Fatal("a failed task should retry after the backoff")
	}

	s.fire(context.Background(), after[0])
	if _, ok := waitEvent(t, s); !ok {
		t.Fatal("expected a second fail-open notification")
	}
	after, _ = s.Store.List()
	if after[0].Failures != 2 {
		t.Fatalf("failures = %d, want consecutive failures to accumulate", after[0].Failures)
	}
}

func TestFireScheduleClearsFailuresOnSuccess(t *testing.T) {
	s := newTestRunner(t)

	past := time.Now().Add(-time.Hour)
	seedTask(t, s, Task{
		ID:          "recovered",
		Prompt:      "poll",
		Schedule:    "every 15s",
		Script:      `echo '{"wake": false}'`,
		Status:      StatusActive,
		CreatedAt:   past.Add(-time.Hour),
		LastRun:     &past,
		Failures:    3,
		LastAttempt: &past,
	})

	tasks, _ := s.Store.List()
	s.fire(context.Background(), tasks[0])

	after, _ := s.Store.List()
	if after[0].Failures != 0 || after[0].LastAttempt != nil {
		t.Fatalf("task = %+v, want the failure state cleared", after[0])
	}
	if after[0].LastRun == nil || !after[0].LastRun.After(past) {
		t.Fatal("a successful gate run should advance the last run")
	}
}

func TestNextScheduleWaitHonorsBackoff(t *testing.T) {
	s := newTestRunner(t)

	now := time.Now()
	seedTask(t, s, Task{
		ID:          "flaky",
		Prompt:      "poll",
		Schedule:    "every 15s",
		Status:      StatusActive,
		CreatedAt:   now.Add(-time.Hour),
		LastRun:     &now,
		Failures:    1,
		LastAttempt: &now,
	})

	if wait := s.nextWait(); wait != scheduleWaitMax {
		t.Fatalf("backed-off wait = %v, want the %v ceiling, not a %v spin", wait, scheduleWaitMax, scheduleWaitMin)
	}
}

func TestNextScheduleWaitTracksShortIntervals(t *testing.T) {
	s := newTestRunner(t)

	if wait := s.nextWait(); wait != scheduleWaitMax {
		t.Fatalf("idle wait = %v, want %v", wait, scheduleWaitMax)
	}

	seedTask(t, s, Task{
		ID:        "fast",
		Prompt:    "poll",
		Schedule:  "every 15s",
		Status:    StatusActive,
		CreatedAt: time.Now(),
	})

	wait := s.nextWait()
	// 15s plus at most 10% jitter, minus the instant since creation.
	if wait < scheduleWaitMin || wait > 17*time.Second {
		t.Fatalf("wait = %v, want roughly the 15s interval", wait)
	}

	seedTask(t, s, Task{
		ID:        "overdue",
		Prompt:    "poll",
		Schedule:  "every 15s",
		Status:    StatusActive,
		CreatedAt: time.Now().Add(-time.Hour),
	})

	if wait := s.nextWait(); wait != scheduleWaitMin {
		t.Fatalf("overdue wait = %v, want the %v floor", wait, scheduleWaitMin)
	}
}

func TestFireScheduleSeqIsUnique(t *testing.T) {
	s := newTestRunner(t)

	job := Task{
		ID:        "repeat",
		Prompt:    "poll",
		Schedule:  "every 1h",
		Status:    StatusActive,
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	seedTask(t, s, job)

	s.fire(context.Background(), job)
	first, _ := waitEvent(t, s)
	tasks, _ := s.Store.List()
	s.fire(context.Background(), tasks[0])
	second, _ := waitEvent(t, s)

	if first.Sequence == second.Sequence {
		t.Fatalf("both fires used seq %d; UIs would drop the second as a duplicate", first.Sequence)
	}
}

func TestFireScheduleSkipsTaskPausedDuringGate(t *testing.T) {
	s := newTestRunner(t)

	job := Task{
		ID:        "paused-mid-gate",
		Prompt:    "poll",
		Schedule:  "every 1h",
		Status:    StatusPaused,
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	seedTask(t, s, job)

	// Simulate fireSchedule starting when the snapshot was active, then seeing
	// StatusPaused when committing the fire mutation.
	job.Status = StatusActive
	s.fire(context.Background(), job)

	if ev, ok := waitEvent(t, s); ok {
		t.Fatalf("paused task should not fire: %+v", ev)
	}
}

func TestRunnerWakesForNewTaskAndStopsOnCancellation(t *testing.T) {
	r := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	seedTask(t, r, Task{ID: "new", Prompt: "wake", Schedule: "every 1h", Status: StatusActive, CreatedAt: time.Now().Add(-2 * time.Hour)})
	select {
	case fire := <-r.events:
		if fire.Task.ID != "new" || fire.Sequence != 1 {
			t.Fatalf("fire = %+v", fire)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new task did not wake the runner")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
}

func TestRunnerCancellationDuringGateDoesNotConsumeTask(t *testing.T) {
	r := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.Gate = func(ctx context.Context, _ Task) (bool, string, error) {
		cancel()
		return true, "", ctx.Err()
	}
	at := time.Now().Add(-time.Minute)
	seedTask(t, r, Task{ID: "once", Schedule: at.Format("2006-01-02T15:04:05"), Status: StatusActive, CreatedAt: at.Add(-time.Hour)})
	r.FireDue(ctx)
	tasks, err := r.Store.List()
	if err != nil || len(tasks) != 1 || tasks[0].FireSeq != 0 || tasks[0].Failures != 0 {
		t.Fatalf("canceled gate consumed task: %+v, %v", tasks, err)
	}
	select {
	case fire := <-r.events:
		t.Fatalf("canceled gate delivered %+v", fire)
	default:
	}
}

func TestRunnerDoesNotCommitStaleGateResult(t *testing.T) {
	for _, change := range []string{"reschedule", "prompt", "script", "paused", "deleted", "already fired"} {
		t.Run(change, func(t *testing.T) {
			r := newTestRunner(t)
			job := Task{ID: "job", Prompt: "original", Schedule: "every 1h", Status: StatusActive, CreatedAt: time.Now().Add(-2 * time.Hour)}
			seedTask(t, r, job)
			var expected []Task
			r.Gate = func(context.Context, Task) (bool, string, error) {
				if err := r.Store.Mutate(func(tasks []Task) ([]Task, error) {
					switch change {
					case "reschedule":
						tasks[0].Schedule = time.Now().Add(time.Hour).Format(time.RFC3339)
					case "prompt":
						tasks[0].Prompt = "replacement"
					case "script":
						tasks[0].Script = "replacement gate"
					case "paused":
						tasks[0].Status = StatusPaused
					case "deleted":
						return nil, nil
					case "already fired":
						tasks[0].FireSeq++
						now := time.Now()
						tasks[0].LastRun = &now
					}
					return tasks, nil
				}); err != nil {
					t.Fatal(err)
				}
				expected, _ = r.Store.List()
				return true, "stale gate output", nil
			}
			r.FireDue(t.Context())
			actual, err := r.Store.List()
			if err != nil || !reflect.DeepEqual(expected, actual) {
				t.Fatalf("stale gate changed current task: got %+v, want %+v, err=%v", actual, expected, err)
			}
			select {
			case fire := <-r.events:
				t.Fatalf("stale gate delivered %+v", fire)
			default:
			}
		})
	}
}

func TestRunnerSkipsTaskPausedWhileEarlierGateRuns(t *testing.T) {
	r := newTestRunner(t)
	for _, id := range []string{"first", "second"} {
		seedTask(t, r, Task{ID: id, Schedule: "every 1h", Status: StatusActive, CreatedAt: time.Now().Add(-2 * time.Hour)})
	}
	r.Gate = func(_ context.Context, task Task) (bool, string, error) {
		if task.ID == "second" {
			t.Error("executed gate for an already paused task")
			return false, "", nil
		}
		err := r.Store.Mutate(func(tasks []Task) ([]Task, error) { tasks[1].Status = StatusPaused; return tasks, nil })
		return false, "", err
	}
	r.FireDue(t.Context())
}

func TestRunnerWithoutDeliveryPreservesTasks(t *testing.T) {
	r := newTestRunner(t)
	r.Deliver = nil
	at := time.Now().Add(-time.Minute)
	seedTask(t, r, Task{ID: "once", Schedule: at.Format(time.RFC3339), Status: StatusActive, CreatedAt: at.Add(-time.Hour)})
	r.Gate = func(context.Context, Task) (bool, string, error) {
		t.Error("gate ran without a delivery callback")
		return true, "", nil
	}
	r.FireDue(t.Context())
	jobs, err := r.Store.List()
	if err != nil || len(jobs) != 1 || jobs[0].FireSeq != 0 {
		t.Fatalf("undeliverable task consumed: %+v, %v", jobs, err)
	}
}
