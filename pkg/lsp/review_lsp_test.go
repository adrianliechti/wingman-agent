package lsp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"go.lsp.dev/protocol"
)

func TestReviewLSPDocumentClosedWhileRestartingIsNotRestored(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := Project{Dir: root, Server: Server{Name: "test", Languages: []string{"go"}}}
	manager := NewManager(root)
	defer manager.Close()
	manager.roots = []projectRoot{project}
	manager.detectedAt = time.Now()
	path := filepath.Join(root, "main.go")
	newSession := func() *Session {
		session := newManagerTestSession(project)
		session.documents = map[string]*document{}
		return session
	}
	dead := newSession()
	if _, err := dead.SyncEditorDocument(ctx, path, "package main // discarded\n", false); err != nil {
		t.Fatal(err)
	}
	dead.alive.Store(false)
	done := make(chan struct{})
	close(done)
	manager.sessions[projectKey(project)] = &sessionEntry{done: done, session: dead}

	restarted := newSession()
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

func TestReviewLSPWaitingCommandHonoursCancellation(t *testing.T) {
	session := &Session{rpc: protocol.UnimplementedServer{}}
	release, err := session.acquireCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := session.ExecuteCommandWithEdits(ctx, protocol.Command{Command: "wait"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting command = %v, want its deadline", err)
	}
	release()
	if next, err := session.acquireCommand(context.Background()); err != nil {
		t.Fatalf("command slot was not released: %v", err)
	} else {
		next()
	}
}
