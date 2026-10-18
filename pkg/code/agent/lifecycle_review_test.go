package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
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
