package task_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
)

func waitEvent(t *testing.T, r *task.Registry) task.Event {
	t.Helper()
	select {
	case ev := <-r.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no completion event")
		return task.Event{}
	}
}

func TestRunAgentLifecycle(t *testing.T) {
	for _, action := range []string{"finish", "fail", "panic", "stop", "cancel parent", "close"} {
		t.Run(action, func(t *testing.T) {
			r := task.NewRegistry()
			defer r.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := make(chan *task.Task, 1)
			release := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				_, _, err := r.RunAgent(ctx, "sync-agent", "inspect", "explore", nil, func(ctx context.Context, tk *task.Task) (string, error) {
					tk.SetActivity("Reading files")
					started <- tk
					select {
					case <-ctx.Done():
						return "", ctx.Err()
					case <-release:
					}
					if action == "fail" {
						return "", errors.New("failed")
					}
					if action == "panic" {
						panic("failed")
					}
					return "report", nil
				})
				done <- err
			}()
			var tk *task.Task
			select {
			case tk = <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("call did not start")
			}
			if got := r.List(); len(got) != 1 || got[0] != tk || tk.Status() != task.StatusRunning || tk.Activity() != "Reading files" {
				t.Fatal("synchronous task is not visible while running")
			}
			if err := r.Relaunch(tk, nil); err == nil {
				t.Fatal("relaunch admitted a running synchronous task")
			}
			switch action {
			case "stop":
				if err := r.Stop(tk.ID); err != nil {
					t.Fatal(err)
				}
			case "cancel parent":
				cancel()
			case "close":
				r.Close()
			default:
				close(release)
			}
			select {
			case err := <-done:
				if (err == nil) != (action == "finish") {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("call did not finish")
			}
			want := task.StatusFailed
			if action == "finish" {
				want = task.StatusDone
			} else if action == "stop" {
				want = task.StatusStopped
			}
			if tk.Status() != want {
				t.Fatalf("status=%s want=%s", tk.Status(), want)
			}
			if running, total := r.Counts(); running != 0 || total != 1 {
				t.Fatalf("running=%d total=%d", running, total)
			}
			select {
			case event := <-r.Events():
				t.Fatalf("inline result also sent as a notification: %+v", event)
			default:
			}
		})
	}
}

func TestTaskPreparationRollsBack(t *testing.T) {
	for _, kind := range []string{"background", "synchronous", "adopted"} {
		for _, failure := range []string{"error", "panic", "shutdown"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				r := task.NewRegistry()
				defer r.Close()
				prepareErr := errors.New("preparation failed")
				prepare := func(*task.Task) error {
					switch failure {
					case "panic":
						panic(prepareErr)
					case "shutdown":
						r.Close()
						return nil
					default:
						return prepareErr
					}
				}
				run := func(context.Context, *task.Task) (string, error) {
					t.Error("ran after failed preparation")
					return "", nil
				}
				var err error
				var recovered any
				func() {
					defer func() { recovered = recover() }()
					switch kind {
					case "background":
						_, err = r.LaunchAgent("prepared", "inspect", "explore", prepare, run)
					case "synchronous":
						_, _, err = r.RunAgent(t.Context(), "prepared", "inspect", "explore", prepare, run)
					case "adopted":
						_, err = r.AdoptAgent("prepared", "inspect", "explore", "done", 0, prepare)
					}
				}()
				if failure == "panic" {
					if recovered != prepareErr {
						t.Fatalf("panic = %v, want %v", recovered, prepareErr)
					}
				} else if recovered != nil || err == nil || (failure == "error" && !errors.Is(err, prepareErr)) {
					t.Fatalf("error = %v, panic = %v", err, recovered)
				}
				if running, total := r.Counts(); running != 0 || total != 0 {
					t.Fatalf("failed admission leaked: running=%d total=%d", running, total)
				}
				if failure != "shutdown" {
					// A failed attempt must release both its identity and its capacity.
					if tk, err := r.AdoptAgent("prepared", "retry", "explore", "done", 0, nil); err != nil || tk == nil {
						t.Fatalf("retry = %v, %v", tk, err)
					}
					for i := 1; i < task.MaxPerSession; i++ {
						if r.Adopt("next", "explore", "done", 0) == nil {
							t.Fatalf("failed preparation consumed capacity: admitted only %d tasks", i)
						}
					}
				}
			})
		}
	}
}

func TestRunAgentRemainsAvailableAtSessionLimit(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()
	for range task.MaxPerSession {
		if r.Adopt("previous", "explore", "done", 0) == nil {
			t.Fatal("could not fill registry")
		}
	}
	tk, out, err := r.RunAgent(t.Context(), "sync", "inspect", "explore", nil, func(_ context.Context, tk *task.Task) (string, error) {
		if tk != nil {
			t.Error("over-capacity run was registered")
		}
		return "inline result", nil
	})
	if tk != nil || err != nil || out != "inline result" {
		t.Fatalf("task=%v output=%q error=%v", tk, out, err)
	}
}

func TestLaunchDeliversCompletionEvent(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()

	launched, err := r.Launch("map auth flow", "explore", func(context.Context, *task.Task) (string, error) {
		return "the report", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if launched.ID == "" || launched.Description != "map auth flow" || launched.AgentType != "explore" {
		t.Fatalf("task = %+v", launched)
	}

	done := waitEvent(t, r)
	if done.Task != launched {
		t.Fatal("event delivered a different task")
	}
	if done.Status != task.StatusDone || done.Result != "the report" {
		t.Fatalf("status = %s, result = %q", done.Status, done.Result)
	}
}

func TestLaunchErrorMarksFailed(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()

	if _, err := r.Launch("d", "explore", func(context.Context, *task.Task) (string, error) {
		return "", errors.New("boom")
	}); err != nil {
		t.Fatal(err)
	}

	done := waitEvent(t, r)
	if done.Status != task.StatusFailed {
		t.Fatalf("status = %s, want failed", done.Status)
	}
	if done.Result != "error: boom" {
		t.Fatalf("result = %q", done.Result)
	}
}

func TestStopCancelsRun(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()

	launched, err := r.Launch("d", "explore", func(ctx context.Context, _ *task.Task) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Stop(launched.ID); err != nil {
		t.Fatal(err)
	}

	done := waitEvent(t, r)
	if done.Status != task.StatusStopped {
		t.Fatalf("status = %s, want stopped", done.Status)
	}

	if err := r.Stop(launched.ID); err == nil {
		t.Fatal("stopping a finished task should error")
	}
	if err := r.Stop("nope"); err == nil {
		t.Fatal("stopping an unknown task should error")
	}
}

func TestConcurrencyCapRejectsLaunch(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()

	release := make(chan struct{})
	for range task.MaxConcurrent {
		if _, err := r.Launch("d", "explore", func(context.Context, *task.Task) (string, error) {
			<-release
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := r.Launch("d", "explore", func(context.Context, *task.Task) (string, error) { return "", nil }); err == nil {
		t.Fatal("launch above the concurrency cap should error")
	}

	close(release)
	for range task.MaxConcurrent {
		waitEvent(t, r)
	}

	if _, err := r.Launch("d", "explore", func(context.Context, *task.Task) (string, error) { return "", nil }); err != nil {
		t.Fatalf("launch after slots freed = %v", err)
	}
}

func TestCloseCancelsRunningAndRejectsLaunch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := task.NewRegistry()
		defer r.Close()
		if _, err := r.Launch("d", "explore", func(ctx context.Context, _ *task.Task) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}); err != nil {
			t.Fatal(err)
		}

		var closers sync.WaitGroup
		for range 4 {
			closers.Go(r.Close)
		}
		closers.Wait()
		synctest.Wait()
		select {
		case <-r.Done():
		default:
			t.Fatal("Close did not signal shutdown")
		}
		if running, _ := r.Counts(); running != 0 {
			t.Fatal("running task did not settle after Close")
		}
		if _, err := r.Launch("d", "explore", func(context.Context, *task.Task) (string, error) {
			t.Error("closed registry ran new work")
			return "", nil
		}); err == nil {
			t.Fatal("launch after Close should error")
		}
		r.Publish(task.Event{ID: "late-event"})
		select {
		case event := <-r.Events():
			t.Fatalf("delivered an event after shutdown: %+v", event)
		default:
		}
	})
}

func TestRelaunchRunsAgainAndKeepsIdentity(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()

	launched, err := r.Launch("d", "explore", func(context.Context, *task.Task) (string, error) {
		return "first", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if waitEvent(t, r).Result != "first" {
		t.Fatal("first run result missing")
	}
	if launched.Seq() != 1 {
		t.Fatalf("seq = %d, want 1", launched.Seq())
	}

	if err := r.Relaunch(launched, func(context.Context, *task.Task) (string, error) {
		return "second", nil
	}); err != nil {
		t.Fatal(err)
	}

	done := waitEvent(t, r)
	if done.Task != launched {
		t.Fatal("relaunch created a different task")
	}
	if done.Status != task.StatusDone || done.Result != "second" || done.Seq != 2 {
		t.Fatalf("status = %s, result = %q, seq = %d", done.Status, done.Result, done.Seq)
	}

	if _, total := r.Counts(); total != 1 {
		t.Fatalf("total tasks = %d, want 1", total)
	}
}

func TestRelaunchRejectsRunningTask(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()

	release := make(chan struct{})
	launched, err := r.Launch("d", "explore", func(context.Context, *task.Task) (string, error) {
		<-release
		return "ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Relaunch(launched, func(context.Context, *task.Task) (string, error) { return "", nil }); err == nil {
		t.Fatal("relaunch of a running task should error")
	}

	close(release)
	waitEvent(t, r)
}

func TestResumeWithoutHookErrors(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()

	launched, err := r.Launch("d", "explore", func(context.Context, *task.Task) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, r)

	if err := launched.Resume("more"); err == nil {
		t.Fatal("resume without a hook should error")
	}
}

func TestAdoptRegistersFinishedRun(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()

	adopted := r.Adopt("sync run", "explore", "the report", 3*time.Second)
	if adopted == nil {
		t.Fatal("adopt returned nil")
	}
	if adopted.Status() != task.StatusDone || adopted.Result() != "the report" {
		t.Fatalf("status = %s, result = %q", adopted.Status(), adopted.Result())
	}
	if adopted.Seq() != 1 {
		t.Fatalf("seq = %d, want 1", adopted.Seq())
	}
	if got := adopted.Elapsed().Round(time.Second); got != 3*time.Second {
		t.Fatalf("elapsed = %s, want 3s", got)
	}
	if r.Get(adopted.ID) != adopted {
		t.Fatal("adopted task not listed in registry")
	}

	select {
	case ev := <-r.Events():
		t.Fatalf("adoption must not emit an event, got %+v", ev)
	default:
	}

	resumed := ""
	adopted.SetResume(func(prompt string) error {
		resumed = prompt
		return nil
	})
	if err := adopted.Resume("follow up"); err != nil || resumed != "follow up" {
		t.Fatalf("resume: err = %v, prompt = %q", err, resumed)
	}
}

func TestAdoptRejectsClosedRegistry(t *testing.T) {
	r := task.NewRegistry()
	r.Close()

	if r.Adopt("d", "explore", "out", time.Second) != nil {
		t.Fatal("closed registry must not adopt")
	}
}

func TestEventDeliveryEvictsOldest(t *testing.T) {
	r := task.NewRegistry()
	defer r.Close()

	for i := range task.MaxPerSession + 1 {
		r.Publish(task.Event{ID: string(rune('A' + i%26)), Seq: i})
	}

	first := waitEvent(t, r)
	if first.Seq != 1 {
		t.Fatalf("first buffered event seq = %d, want oldest evicted (seq 1 kept)", first.Seq)
	}

	drained := 1
	for {
		select {
		case ev := <-r.Events():
			drained++
			if drained == task.MaxPerSession && ev.Seq != task.MaxPerSession {
				t.Fatalf("last event seq = %d, want %d", ev.Seq, task.MaxPerSession)
			}
		default:
			if drained != task.MaxPerSession {
				t.Fatalf("drained %d events, want %d", drained, task.MaxPerSession)
			}
			return
		}
	}
}
