package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

func TestLoadCannotResurrectSessionDuringDeletion(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	const id = "delete-and-load"
	closing, release := make(chan struct{}), make(chan struct{})
	releaseCleanup := sync.OnceFunc(func() { close(release) })
	var once sync.Once
	var a *Agent
	a = New(ws, Config{Options: Options{DisableShell: true}, Config: harness.Config{
		Hooks: hook.Hooks{SessionEnd: []hook.SessionEnd{func(context.Context, string) {
			// End hooks may invoke the compatibility save API. The session was
			// already removed, so this must remain a no-op during deletion.
			if err := a.Save(id); err != nil {
				t.Error(err)
			}
			once.Do(func() {
				close(closing)
				<-release
			})
		}}},
	}}, nil)
	defer a.Close()
	defer releaseCleanup()
	if err := session.Save(a.sessionsDir, id, harness.State{Messages: []harness.Message{{Role: harness.RoleUser, Content: []harness.Content{{Text: "saved"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.LoadSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- a.DeleteSession(t.Context(), id) }()
	select {
	case <-closing:
	case <-time.After(time.Second):
		t.Fatal("delete did not reach cleanup")
	}
	loaded := make(chan error, 1)
	go func() { loaded <- a.LoadSession(t.Context(), id) }()
	var returned bool
	select {
	case err := <-loaded:
		returned = true
		if err == nil {
			t.Error("load succeeded while the session was being deleted")
		}
	case <-time.After(50 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := a.LoadSession(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancelled load during cleanup = %v, want deadline exceeded", err)
	}
	releaseCleanup()
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if !returned {
		if err := <-loaded; err == nil {
			t.Error("load succeeded after deletion")
		}
	}
	if a.session(id) != nil {
		t.Fatal("deleted session was resurrected in memory")
	}
}

func TestSessionWorkersWaitForRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := New(newOptionsTestWorkspace(t), Config{Options: Options{DisableShell: true}}, nil)
		defer a.Close()
		s, err := a.buildSession("not-published")
		if err != nil {
			t.Fatal(err)
		}
		defer s.close()
		if err := s.schedules.Mutate(func(tasks []schedule.Task) ([]schedule.Task, error) {
			return append(tasks, schedule.Task{ID: "due", Prompt: "check", Schedule: "every 1h", Status: schedule.StatusActive, CreatedAt: time.Now().Add(-2 * time.Hour)}), nil
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		select {
		case event := <-s.tasks.Events():
			t.Fatalf("unpublished session ran background work: %+v", event)
		default:
		}
		jobs, err := s.schedules.List()
		if err != nil || len(jobs) != 1 || jobs[0].FireSeq != 0 {
			t.Fatalf("unpublished schedule changed: %+v, %v", jobs, err)
		}
		if err := a.registerSession(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-s.tasks.Events():
			if event.ID != "due" || a.session(s.id) != s {
				t.Fatalf("published event = %+v", event)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("published session did not start its workers")
		}
	})
}

type blockedScheduleStore struct {
	schedule.ObservableStore
	entered chan struct{}
	release chan struct{}
}

func (s *blockedScheduleStore) Mutate(fn func([]schedule.Task) ([]schedule.Task, error)) error {
	close(s.entered)
	<-s.release
	return s.ObservableStore.Mutate(fn)
}

func TestSessionCloseJoinsScheduleWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := New(newOptionsTestWorkspace(t), Config{Options: Options{DisableShell: true}}, nil)
		defer a.Close()
		s, err := a.buildSession("closing-schedule")
		if err != nil {
			t.Fatal(err)
		}
		store := &blockedScheduleStore{ObservableStore: schedule.NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
		unblock := sync.OnceFunc(func() { close(store.release) })
		defer unblock()
		s.schedules = store
		if err := store.ObservableStore.Mutate(func(tasks []schedule.Task) ([]schedule.Task, error) {
			return append(tasks, schedule.Task{ID: "due", Schedule: "every 1h", Status: schedule.StatusActive, CreatedAt: time.Now().Add(-2 * time.Hour)}), nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := a.registerSession(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		select {
		case <-store.entered:
		default:
			t.Fatal("scheduler did not reach store write")
		}
		closed := make(chan struct{})
		go func() { defer close(closed); a.Close() }()
		synctest.Wait()
		select {
		case <-closed:
			t.Error("close returned while the scheduler could still write")
		default:
		}
		unblock()
		<-closed
		jobs, err := store.List()
		if err != nil || len(jobs) != 1 || jobs[0].FireSeq != 0 {
			t.Fatalf("canceled write changed schedule: %+v, %v", jobs, err)
		}
	})
}

func TestCanceledNewSessionDoesNotPublish(t *testing.T) {
	a := New(newOptionsTestWorkspace(t), Config{Options: Options{DisableShell: true}}, nil)
	defer a.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.NewSession(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewSession error = %v", err)
	}
	if len(a.sessions) != 0 {
		t.Fatal("canceled request published a session")
	}
}
