//go:build unix

package server

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Bug: resourceRead opens a path before checking that it is a regular file, so reading a FIFO in the workspace blocks the request forever; it must fail with InvalidParams without opening it for reading.
func TestReviewWorkspaceResourceReadDoesNotBlockOnFIFO(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip("mkfifo unavailable:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := reviewWorkspaceResourceCall(h, "resourceRead", map[string]any{"uri": h.fileURI("pipe")})
		done <- err
	}()
	select {
	case err := <-done:
		reviewWorkspaceResourceCode(t, err, -32602)
		return
	case <-time.After(2 * time.Second):
		t.Error("resourceRead blocked on a FIFO")
	}
	// Release the blocked reader so the test leaves no goroutine behind.
	for {
		if writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			writer.Close()
		}
		select {
		case <-done:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}
