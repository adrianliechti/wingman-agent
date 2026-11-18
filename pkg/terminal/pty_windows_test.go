//go:build windows

package terminal

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestWindowsConPTYRunsCommand starts a real command under the ConPTY backend
// and checks that output flows back and the process exits. It is the Windows
// counterpart of the Unix workspace-PTY test.
func TestWindowsConPTYRunsCommand(t *testing.T) {
	if !Supported() {
		t.Skip("pty not supported")
	}

	root := t.TempDir()
	manager := NewManager(root)
	defer manager.Close()

	marker := "wingman-conpty-ok"
	session, err := manager.CreateCommand(CommandSpec{
		Path:  os.Getenv("ComSpec"),
		Args:  []string{"/c", "echo " + marker},
		Dir:   root,
		Title: "ConPTY command",
	}, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if session.ProcessID() <= 0 {
		t.Fatalf("process id = %d, want > 0", session.ProcessID())
	}

	snapshot, output, cancel := session.Subscribe()
	defer cancel()

	var builder strings.Builder
	builder.Write(snapshot)

	deadline := time.After(15 * time.Second)
	for !strings.Contains(builder.String(), marker) {
		select {
		case chunk, ok := <-output:
			if !ok {
				if strings.Contains(builder.String(), marker) {
					return
				}
				t.Fatalf("output closed before marker; got %q", builder.String())
			}
			builder.Write(chunk)
		case <-deadline:
			t.Fatalf("timed out waiting for marker; got %q", builder.String())
		}
	}

	select {
	case <-session.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not exit after command completed")
	}
}

// TestWindowsShellReportsTheCommandItRuns checks that an interactive shell is
// busy while it runs a command and not while it waits at its prompt.
func TestWindowsShellReportsTheCommandItRuns(t *testing.T) {
	if !Supported() {
		t.Skip("pty not supported")
	}

	root := t.TempDir()
	manager := NewManager(root)
	defer manager.Close()

	session, err := manager.CreateCommand(CommandSpec{
		Path:  os.Getenv("ComSpec"),
		Dir:   root,
		Title: "ConPTY shell",
	}, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	time.Sleep(500 * time.Millisecond)
	if session.HasForegroundProcess() {
		t.Fatal("a shell at its prompt reports a command")
	}
	if err := session.Write([]byte("ping -n 30 127.0.0.1\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !session.HasForegroundProcess() {
		if time.Now().After(deadline) {
			t.Fatal("a shell that runs a command does not report it")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
