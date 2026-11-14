package task

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitTimeoutCancellationAndUnknownID(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	tk, err := r.Launch("blocked", "explore", func(ctx context.Context, _ *Task) (string, error) { <-ctx.Done(); return "", ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	infos, timedOut, err := r.Wait(t.Context(), []string{tk.ID}, true, 10*time.Millisecond)
	if err != nil || !timedOut || len(infos) != 1 || infos[0].Status != StatusRunning {
		t.Fatalf("timeout: %+v %v %v", infos, timedOut, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, timedOut, err := r.Wait(ctx, []string{tk.ID}, true, time.Second); !errors.Is(err, context.Canceled) || timedOut {
		t.Fatalf("cancellation=%v timeout=%v", err, timedOut)
	}
	if _, _, err := r.Wait(t.Context(), []string{"missing"}, true, time.Second); err == nil {
		t.Fatal("unknown id accepted")
	}
}

// drainEvents collects the completion notifications published so far.
func drainEvents(r *Registry, settle time.Duration) []Event {
	var events []Event
	deadline := time.After(settle)
	for {
		select {
		case ev := <-r.Events():
			events = append(events, ev)
		case <-deadline:
			return events
		}
	}
}

func TestWaitDeliversEachResultExactlyOnce(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	release := map[string]chan struct{}{"fast": make(chan struct{}), "slow": make(chan struct{})}
	launch := func(name string) *Task {
		t.Helper()
		tk, err := r.Launch(name, "explore", func(ctx context.Context, _ *Task) (string, error) {
			select {
			case <-release[name]:
				return name + " report", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	fast, slow := launch("fast"), launch("slow")

	// The wait is blocked when fast finishes, so its report comes back inline.
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release["fast"])
	}()
	infos, timedOut, err := r.Wait(t.Context(), []string{fast.ID, slow.ID}, false, 5*time.Second)
	if err != nil || timedOut || infos[0].Result != "fast report" || infos[1].Status != StatusRunning {
		t.Fatalf("wait any: %+v timedOut=%v err=%v", infos, timedOut, err)
	}
	if events := drainEvents(r, 50*time.Millisecond); len(events) != 0 {
		t.Fatalf("result returned inline was also published: %+v", events)
	}

	// Nobody is waiting on slow any more, so it notifies as usual.
	close(release["slow"])
	events := drainEvents(r, 200*time.Millisecond)
	if len(events) != 1 || events[0].ID != slow.ID || events[0].Result != "slow report" {
		t.Fatalf("unwaited completion not published once: %+v", events)
	}
}

func TestWaitThatFailsStillPublishesFinishedResults(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	done := make(chan struct{})
	finished, err := r.Launch("finished", "explore", func(ctx context.Context, _ *Task) (string, error) {
		<-done
		return "finished report", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := r.Launch("blocked", "explore", func(ctx context.Context, _ *Task) (string, error) { <-ctx.Done(); return "", ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}

	// The first task finishes while a wait-for-all is blocked, then the wait
	// is interrupted before it can return anything.
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(done)
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	if _, _, err := r.Wait(ctx, []string{finished.ID, blocked.ID}, true, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v", err)
	}
	events := drainEvents(r, 200*time.Millisecond)
	if len(events) != 1 || events[0].ID != finished.ID || events[0].Result != "finished report" {
		t.Fatalf("result lost when the wait was interrupted: %+v", events)
	}
}

func TestWaitTimeoutReturnsFinishedResultsInline(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	done := make(chan struct{})
	finished, err := r.Launch("finished", "explore", func(ctx context.Context, _ *Task) (string, error) {
		<-done
		return "finished report", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := r.Launch("blocked", "explore", func(ctx context.Context, _ *Task) (string, error) { <-ctx.Done(); return "", ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(done)
	}()
	infos, timedOut, err := r.Wait(t.Context(), []string{finished.ID, blocked.ID}, true, 150*time.Millisecond)
	if err != nil || !timedOut || infos[0].Result != "finished report" || infos[1].Status != StatusRunning {
		t.Fatalf("timeout: %+v timedOut=%v err=%v", infos, timedOut, err)
	}
	if events := drainEvents(r, 50*time.Millisecond); len(events) != 0 {
		t.Fatalf("result returned on timeout was also published: %+v", events)
	}
}
