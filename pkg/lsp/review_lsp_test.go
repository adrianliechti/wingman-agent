package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.lsp.dev/protocol"
)

func TestReviewLSPDocumentClosedWhileRestartingIsNotRestored(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	manager := newTestManager(root)
	defer manager.Close()
	path := filepath.Join(root, "main.go")
	dead := newManagerTestSession(root)
	if _, err := dead.SyncEditorDocument(ctx, path, "package main // discarded\n", false); err != nil {
		t.Fatal(err)
	}
	dead.alive.Store(false)
	done := make(chan struct{})
	close(done)
	manager.sessions[testServer.Name] = &sessionEntry{done: done, session: dead}

	restarted := newManagerTestSession(root)
	// An unnecessary didOpen would fail on the unimplemented test server.
	restarted.capabilities.TextDocumentSync = protocol.TextDocumentSyncKindFull
	started, release := make(chan struct{}), make(chan struct{})
	manager.connect = func(context.Context, string, Server) (*Session, error) {
		close(started)
		<-release
		return restarted, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := manager.GetSession(ctx, path)
		result <- err
	}()
	<-started
	if err := manager.CloseDocument(ctx, path); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if content, open := restarted.DocumentContent(path); open {
		t.Fatalf("a document closed during the restart was restored: %q", content)
	}
}

type blockingDocumentRPC struct {
	protocol.UnimplementedServer
	opening, resumeOpen, closing, resumeClose chan struct{}
}

func (r *blockingDocumentRPC) DidOpen(context.Context, *protocol.DidOpenTextDocumentParams) error {
	close(r.opening)
	<-r.resumeOpen
	return nil
}

func (r *blockingDocumentRPC) DidClose(context.Context, *protocol.DidCloseTextDocumentParams) error {
	close(r.closing)
	<-r.resumeClose
	return nil
}

func TestRestartWaitsForCloseReceivedDuringReplay(t *testing.T) {
	root := t.TempDir()
	manager := newTestManager(root)
	defer manager.Close()
	path := filepath.Join(root, "main.go")
	dead := newManagerTestSession(root)
	if _, err := dead.SyncEditorDocument(t.Context(), path, "draft", false); err != nil {
		t.Fatal(err)
	}
	dead.alive.Store(false)
	done := make(chan struct{})
	close(done)
	manager.sessions[testServer.Name] = &sessionEntry{done: done, session: dead}
	rpc := &blockingDocumentRPC{opening: make(chan struct{}), resumeOpen: make(chan struct{}), closing: make(chan struct{}), resumeClose: make(chan struct{})}
	restarted := newManagerTestSession(root)
	restarted.capabilities.TextDocumentSync = protocol.TextDocumentSyncKindFull
	restarted.rpc = rpc
	manager.connect = func(context.Context, string, Server) (*Session, error) { return restarted, nil }
	result := make(chan error, 1)
	go func() {
		_, err := manager.GetSession(t.Context(), path)
		result <- err
	}()
	<-rpc.opening
	if err := manager.CloseDocument(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	close(rpc.resumeOpen)
	<-rpc.closing
	if _, active := manager.ActiveSession(path); active {
		t.Error("restart accepted new requests before closing the restored document")
	}
	close(rpc.resumeClose)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestRestartRestoresEditorBuffersAndRereadsDiskAfterFailedAttempt(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	manager := newTestManager(root)
	defer manager.Close()
	original := newManagerTestSession(root)
	manager.connect = func(context.Context, string, Server) (*Session, error) { return original, nil }
	changed, deleted := filepath.Join(root, "changed.go"), filepath.Join(root, "deleted.go")
	draft, closed := filepath.Join(root, "draft.go"), filepath.Join(root, "closed.go")
	if _, err := manager.GetSession(ctx, changed); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{changed, deleted} {
		if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := original.OpenDocument(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{draft, closed} {
		if _, err := original.SyncEditorDocument(ctx, path, "unsaved", false); err != nil {
			t.Fatal(err)
		}
	}
	lookup := manager.lookPath
	manager.lookPath = func(string) string { return "" }
	clear(manager.commands)
	if got, err := manager.GetSession(ctx, draft); err != nil || got != original {
		t.Fatalf("request after executable moved = %p, %v", got, err)
	}
	original.alive.Store(false)
	if err := manager.CloseDocument(ctx, closed); err != nil {
		t.Fatal(err)
	}
	manager.lookPath = lookup
	if err := os.WriteFile(changed, []byte("current"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("startup failed")
	manager.connect = func(context.Context, string, Server) (*Session, error) { return nil, failure }
	if _, err := manager.GetSession(ctx, draft); !errors.Is(err, failure) {
		t.Fatalf("first restart = %v, want %v", err, failure)
	}
	manager.connect = func(context.Context, string, Server) (*Session, error) { return newManagerTestSession(root), nil }
	restarted, err := manager.GetSession(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{changed: "current", draft: "unsaved", deleted: "", closed: ""} {
		if got, open := restarted.DocumentContent(path); got != want || open != (want != "") {
			t.Errorf("%s after restart = %q, open=%t", filepath.Base(path), got, open)
		}
	}
}

func TestReviewLSPWaitingCommandHonoursCancellation(t *testing.T) {
	session := &Session{rpc: protocol.UnimplementedServer{}}
	release, err := session.acquireCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := session.ExecuteCommand(ctx, protocol.Command{Command: "wait"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting command = %v, want its deadline", err)
	}
	release()
	if next, err := session.acquireCommand(context.Background()); err != nil {
		t.Fatalf("command slot was not released: %v", err)
	} else {
		next()
	}
}
