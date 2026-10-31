package dap

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Breakpoints toggled during a prelaunch build, and after configuration but
// before the session becomes active, must reach the adapter.
func TestManagerAppliesBreakpointsChangedDuringStart(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/test\n")
	source := filepath.Join(root, "main.go")
	writeTestFile(t, source, "package main\n")
	adapterPath := filepath.Join(root, "dlv")
	writeTestFile(t, adapterPath, "adapter\n")
	if err := os.Chmod(adapterPath, 0o755); err != nil {
		t.Fatal(err)
	}
	building, configured := make(chan struct{}), make(chan struct{})
	release, finish := make(chan struct{}), make(chan struct{})
	adapters := make(chan *fakeAdapter, 1)
	manager := newManager(root, []AdapterDescriptor{{
		Name: "delve", Language: "Go", Command: "dlv", Markers: []string{"go.mod"}, SourceExtensions: []string{".go"},
	}}, func(string) string { return adapterPath }, func(ctx context.Context, id string, plan Plan, options StartOptions) (*Session, error) {
		close(building)
		<-release
		client, server := net.Pipe()
		adapter := newFakeAdapter(t, server)
		adapters <- adapter
		go adapter.serve()
		session := newConnectedSession(id, plan, client)
		session.hooks = options.hooks
		err := session.initializeAndLaunch(ctx, options)
		close(configured)
		<-finish
		return session, err
	})
	defer manager.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan error, 1)
	go func() {
		_, err := manager.Start(ctx, StartOptions{Adapter: "delve"})
		started <- err
	}()
	<-building
	pending, err := manager.SetBreakpoints(ctx, source, []SourceBreakpoint{{Line: 5}})
	if err != nil || len(pending) != 1 || pending[0].Reason != "pending" || pending[0].ID == 0 {
		t.Fatalf("breakpoints during build = %+v, %v", pending, err)
	}
	close(release)
	<-configured
	adapter := <-adapters
	if lines := adapter.lastBreakpointLines(); !slices.Equal(lines, []int{5}) {
		t.Fatalf("configuration sent %v, want the edit made during the build", lines)
	}
	again, err := manager.SetBreakpoints(ctx, source, []SourceBreakpoint{{Line: 5}, {Line: 9}})
	if err != nil || len(again) != 2 || again[0].ID != pending[0].ID {
		t.Fatalf("breakpoint ids must stay stable: %+v, %v", again, err)
	}
	close(finish)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if lines := adapter.lastBreakpointLines(); !slices.Equal(lines, []int{5, 9}) {
		t.Fatalf("adapter breakpoints = %v, want the edit made before activation", lines)
	}
	resolved := manager.ResolvedSourceBreakpoints(source)
	if len(resolved) != 2 || !resolved[0].Verified || !resolved[1].Verified || resolved[0].ID != pending[0].ID {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestOutputBufferStreamsByUTF16Offset(t *testing.T) {
	var buffer outputBuffer
	buffer.append("stdout", "a😀\n")
	buffer.append("telemetry", `{"event":"launch"}`)
	buffer.append("stderr", "warn\n")
	chunks, end := buffer.since(0)
	if len(chunks) != 2 || chunks[0].Category != "stdout" || chunks[1].Category != "stderr" || chunks[1].Offset != 4 || end != 9 {
		t.Fatalf("chunks = %+v, end = %d", chunks, end)
	}
	if chunks, _ := buffer.since(4); len(chunks) != 1 || chunks[0].Output != "warn\n" {
		t.Fatalf("output since offset 4 = %+v", chunks)
	}
	if text := buffer.String(); strings.Contains(text, "launch") || text != "a😀\nwarn\n" {
		t.Fatalf("output = %q; telemetry must not be shown and categories not prefixed", text)
	}

	buffer.append("stdout", strings.Repeat("é", maxOutputBytes/2+10))
	chunks, end = buffer.since(4)
	if len(chunks) == 0 || chunks[0].Offset <= 4 {
		t.Fatalf("an offset before the retained window must return everything retained: %+v", chunks)
	}
	retained := int64(0)
	for _, chunk := range chunks {
		retained += utf16Length(chunk.Output)
	}
	if chunks[0].Offset+retained != end || !strings.HasPrefix(buffer.String(), "[earlier debugger output truncated]") {
		t.Fatalf("offsets do not cover the retained text: first %d + %d != %d", chunks[0].Offset, retained, end)
	}
}
