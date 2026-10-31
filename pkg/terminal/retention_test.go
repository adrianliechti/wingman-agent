//go:build !windows

package terminal

import (
	"testing"
	"time"
)

func TestSlowSubscriberReceivesAWholeBurst(t *testing.T) {
	manager := NewManager(t.TempDir())
	defer manager.Close()
	session, err := manager.CreateCommand(CommandSpec{Path: "/bin/sh", Args: []string{"-c", "yes 0123456789 | head -c 200000"}}, DefaultCols, DefaultRows)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, output, cancel := session.Subscribe()
	defer cancel()
	<-session.Done() // the burst finishes before this subscriber reads anything
	received := len(snapshot)
	for chunk := range output {
		received += len(chunk)
	}
	if received < 200000 {
		t.Fatalf("received %d bytes; a burst within the scrollback must not cut the subscriber off", received)
	}
}

func TestExitedSessionsAreRetainedButBounded(t *testing.T) {
	manager := NewManager(t.TempDir())
	defer manager.Close()
	running, err := manager.CreateCommand(CommandSpec{Path: "/bin/sh", Args: []string{"-c", "read answer"}}, DefaultCols, DefaultRows)
	if err != nil {
		t.Fatal(err)
	}
	var first *Session
	for i := range maxRetainedExited + 2 {
		session, err := manager.CreateCommand(CommandSpec{Path: "/bin/sh", Args: []string{"-c", "true"}}, DefaultCols, DefaultRows)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = session
		}
		select {
		case <-session.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("command did not exit")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(manager.List()) > maxRetainedExited+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(manager.List()); got != maxRetainedExited+1 {
		t.Fatalf("sessions = %d, want %d retained exited plus the running one", got, maxRetainedExited)
	}
	if manager.Get(first.ID()) != nil || manager.Get(running.ID()) == nil {
		t.Fatal("pruning must drop the oldest exited session and keep running ones")
	}
	manager.mu.Lock()
	count := manager.runningLocked()
	manager.mu.Unlock()
	if count != 1 {
		t.Fatalf("running = %d, want only the running session counted toward MaxSessions", count)
	}
}
