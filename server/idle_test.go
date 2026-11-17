package server

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/terminal"
)

// A host is idle while no client follows it and nothing works.
func TestServerIsIdleWithoutClients(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	if !s.Idle() {
		t.Fatal("a new host is not idle")
	}
	client := protocolTestClient(t, endpoint, "idle-test")
	if s.Idle() {
		t.Fatal("a host is idle while a client follows it")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_ = client.Shutdown(ctx)
	for !s.Idle() {
		if ctx.Err() != nil {
			t.Fatal("the host stays busy after its client left")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A terminal that runs a command keeps the host working, even one started
// with its command, which is its own foreground process.
func TestServerIsNotIdleWhileATerminalRunsACommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	s, _ := reviewWorkspaceTerminalServer(t)
	session, err := s.Project.terminals.CreateCommand(terminal.CommandSpec{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if s.Idle() {
		t.Fatal("the host is idle while a terminal runs a command")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for !s.Idle() {
		if ctx.Err() != nil {
			t.Fatal("the host stays busy after the command ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
