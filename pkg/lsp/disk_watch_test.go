package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"go.lsp.dev/protocol"
)

type diskEventRecorder struct {
	protocol.UnimplementedServer
	events chan protocol.FileEvent
}

func (r diskEventRecorder) DidChangeWatchedFiles(ctx context.Context, p *protocol.DidChangeWatchedFilesParams) error {
	for _, e := range p.Changes {
		select {
		case r.events <- e:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestSessionWatchesOnlyRegisteredFilesAndDiskDocuments(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "src")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	disk := filepath.Join(dir, "one.go")
	watched := filepath.Join(dir, "two.go")
	write(disk, "one")
	write(watched, "one")
	rpc := diskEventRecorder{events: make(chan protocol.FileEvent, 32)}
	ctx := t.Context()
	s := &Session{root: root, rpc: rpc, documents: make(map[string]*document), disk: newDiskWatches(ctx, root)}
	t.Cleanup(s.stopDiskWatches)
	client := &sessionClient{session: s}
	assertWatch := func(dirs ...watchDir) {
		t.Helper()
		s.watchMu.Lock()
		defer s.watchMu.Unlock()
		if len(s.disk.active) != len(dirs) {
			t.Fatalf("active watch count = %d, want %d", len(s.disk.active), len(dirs))
		}
		for _, dir := range dirs {
			if s.disk.active[dir] == nil {
				t.Fatalf("missing watch for %+v", dir)
			}
		}
	}
	waitEvent := func(path string, kind protocol.FileChangeType) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case event := <-rpc.events:
				if filepath.Ext(string(event.URI)) != ".go" {
					t.Fatalf("unexpected notification %+v", event)
				}
				if string(event.URI) == fileuri.FromPath(path) && event.Type == kind {
					return
				}
			case <-timer.C:
				t.Fatalf("timed out waiting for %s type %v", path, kind)
			}
		}
	}
	assertWatch()
	if _, err := s.OpenDocument(ctx, disk); err != nil {
		t.Fatal(err)
	}
	assertWatch(watchDir{dir, false})
	write(disk, "two")
	for deadline := time.Now().Add(5 * time.Second); ; {
		if content, _ := s.DocumentContent(disk); content == "two" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disk document did not refresh without a registration")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := client.RegisterCapability(ctx, &protocol.RegistrationParams{Registrations: []protocol.Registration{{ID: "go", Method: watchedFilesMethod, RegisterOptions: protocol.LSPAny(`{"watchers":[{"globPattern":"src/**/*.go"}]}`)}}}); err != nil {
		t.Fatal(err)
	}
	assertWatch(watchDir{dir, true})
	// A failed replacement must preserve both the subscription and its glob.
	write(filepath.Join(root, "blocked"), "regular file")
	invalid := protocol.Registration{
		ID: "go", Method: watchedFilesMethod,
		RegisterOptions: protocol.LSPAny(`{"watchers":[{"globPattern":"*.tmp"},{"globPattern":"blocked/sub/*.go"}]}`),
	}
	if err := client.RegisterCapability(ctx, &protocol.RegistrationParams{Registrations: []protocol.Registration{invalid}}); err == nil {
		t.Fatal("registered a watch below a regular file")
	}
	assertWatch(watchDir{dir, true})
	// Claiming identical text still transfers ownership to the editor.
	if _, err := s.SyncEditorDocument(ctx, disk, "two", true); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dir, "ignored.txt"), "noise")
	write(watched, "two")
	waitEvent(watched, protocol.FileChangeTypeChanged)
	created := filepath.Join(dir, "new.go")
	write(created, "one")
	waitEvent(created, protocol.FileChangeTypeCreated)
	write(created, "two")
	waitEvent(created, protocol.FileChangeTypeChanged)
	if err := os.Remove(created); err != nil {
		t.Fatal(err)
	}
	waitEvent(created, protocol.FileChangeTypeDeleted)
	// A single directory rename can bring an already populated tree into scope.
	staged := filepath.Join(root, "staged")
	if err := os.Mkdir(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(staged, "nested.go"), "initial")
	nested := filepath.Join(dir, "generated", "nested.go")
	if err := os.Rename(staged, filepath.Dir(nested)); err != nil {
		t.Fatal(err)
	}
	waitEvent(nested, protocol.FileChangeTypeCreated)
	write(nested, "updated")
	waitEvent(nested, protocol.FileChangeTypeChanged)
	if err := os.Rename(filepath.Dir(nested), staged); err != nil {
		t.Fatal(err)
	}
	waitEvent(nested, protocol.FileChangeTypeDeleted)
	write(disk, "external")
	waitEvent(disk, protocol.FileChangeTypeChanged)
	if content, _ := s.DocumentContent(disk); content != "two" {
		t.Fatalf("editor was replaced by %q", content)
	}
	if err := client.UnregisterCapability(ctx, &protocol.UnregistrationParams{Unregisterations: []protocol.Unregistration{{ID: "go", Method: watchedFilesMethod}}}); err != nil {
		t.Fatal(err)
	}
	assertWatch()
}
