package external

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
)

func TestAsyncConcurrentCloseRejectsWorkWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		async := NewAsync(func(hook.Runtime, []string) { t.Error("canceled hook delivered output") })
		async.start(hook.Runtime{}, func(ctx context.Context) []string {
			close(started)
			<-ctx.Done()
			<-release
			return []string{"late output"}
		})
		<-started

		closed := make(chan struct{}, 4)
		var closers sync.WaitGroup
		for range cap(closed) {
			closers.Go(func() {
				async.Close()
				closed <- struct{}{}
			})
		}
		synctest.Wait()
		async.start(hook.Runtime{}, func(context.Context) []string {
			t.Error("hook started during shutdown")
			return nil
		})
		select {
		case <-closed:
			t.Fatal("Close returned before the hook finished")
		default:
		}
		close(release)
		closers.Wait()
		async.Close()
	})
}
