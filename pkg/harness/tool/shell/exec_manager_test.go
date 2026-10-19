package shell

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestExecManagerConcurrentCloseReleasesSessions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewExecManager(func(ExecExit) { t.Error("closed manager notified an exit") })
		var cancellations atomic.Int32
		var sessions []*execSession
		for range 3 {
			s := &execSession{done: make(chan struct{}), backgrounded: true}
			s.cancel = func() {
				cancellations.Add(1)
				// Cancellation may reenter the manager during process cleanup.
				manager.remove(s.id)
				manager.notifyExit(s)
			}
			if _, err := manager.add(s); err != nil {
				t.Fatal(err)
			}
			sessions = append(sessions, s)
		}
		var closers sync.WaitGroup
		for range 4 {
			closers.Go(manager.Close)
		}
		closers.Wait()
		if got := cancellations.Load(); got != int32(len(sessions)) {
			t.Fatalf("canceled %d sessions, want %d", got, len(sessions))
		}
		if got := manager.BackgroundSessions(); len(got) != 0 {
			t.Fatalf("closed manager retained sessions: %+v", got)
		}
		for _, s := range sessions {
			if manager.get(s.id) != nil {
				t.Fatal("closed session remained available")
			}
		}
		rejected := &execSession{cancel: func() { cancellations.Add(1) }}
		if _, err := manager.add(rejected); err == nil || cancellations.Load() != 4 {
			t.Fatalf("new session was not rejected and canceled: %v", err)
		}
	})
}
