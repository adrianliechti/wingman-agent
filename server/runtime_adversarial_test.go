package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestBackendStartupIsSharedWithoutBlockingResidentBackends(t *testing.T) {
	s, resident, _ := newProtocolServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	create := func(context.Context) (code.Agent, error) {
		calls.Add(1)
		close(started)
		<-release
		return &protocolAgent{}, nil
	}
	results := make(chan *backendRuntime, 24)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Go(func() {
			b, err := s.resolveBackend("slow", create)
			if err != nil {
				t.Error(err)
			}
			results <- b
		})
	}
	<-started
	fast := make(chan *backendRuntime, 1)
	go func() { b, _ := s.backend("one"); fast <- b }()
	select {
	case b := <-fast:
		if b != resident {
			t.Error("resident backend changed")
		}
	case <-time.After(time.Second):
		t.Error("startup blocked another backend")
	}
	close(release)
	wg.Wait()
	close(results)
	var first *backendRuntime
	for b := range results {
		if first == nil {
			first = b
		}
		if b != first {
			t.Error("callers received different runtimes")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("created %d agents", calls.Load())
	}
}

func TestBackendStartupAfterShutdownIsDisposedAndJoined(t *testing.T) {
	s, _, base := newProtocolServer(t)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var disposed atomic.Bool
	a := &lifecycleAgent{protocolAgent: base, closeAgent: func() error { disposed.Store(true); return nil }}
	resolved := make(chan error, 1)
	go func() {
		_, err := s.resolveBackend("late", func(ctx context.Context) (code.Agent, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-release
			return a, nil
		})
		resolved <- err
	}()
	<-started
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	<-cancelled
	select {
	case <-closed:
		t.Error("workspace closed during backend startup")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-resolved; err == nil {
		t.Error("published backend after shutdown")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join startup")
	}
	if !disposed.Load() {
		t.Fatal("late backend leaked")
	}
}

func TestFailedBackendStartupCanBeRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _, _ := newProtocolServer(t)
		release := make(chan struct{})
		failure := errors.New("startup failed")
		var calls, disposals atomic.Int32
		create := func(context.Context) (code.Agent, error) {
			calls.Add(1)
			<-release
			return &lifecycleAgent{protocolAgent: &protocolAgent{}, closeAgent: func() error {
				disposals.Add(1)
				return nil
			}}, failure
		}
		results := make(chan error, 2)
		for range cap(results) {
			go func() {
				backend, err := s.resolveBackend("retry", create)
				if backend != nil {
					t.Error("failed startup published a backend")
				}
				results <- err
			}()
		}
		synctest.Wait()
		close(release)
		for range cap(results) {
			if err := <-results; !errors.Is(err, failure) {
				t.Fatalf("shared startup error = %v, want %v", err, failure)
			}
		}
		if calls.Load() != 1 || disposals.Load() != 1 {
			t.Fatalf("failed startup created %d agents and disposed %d, want 1 each", calls.Load(), disposals.Load())
		}

		var want *backendRuntime
		for range 2 {
			backend, err := s.resolveBackend("retry", func(context.Context) (code.Agent, error) {
				calls.Add(1)
				return &protocolAgent{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if want == nil {
				want = backend
			} else if backend != want {
				t.Fatal("successful retry was not reused")
			}
		}
		if calls.Load() != 2 {
			t.Fatalf("startup attempts = %d, want 2", calls.Load())
		}
	})
}
