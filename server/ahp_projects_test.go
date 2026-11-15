package server

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// Exercise project ownership without starting tools, providers or listeners.
func projectLifecycleHost(t *testing.T, root string) *Host {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	h := &Host{ctx: ctx, cancel: cancel, ahp: newAHPHost()}
	h.ahp.roots = []string{root}
	h.ahp.open = func(dir string) (*Server, error) {
		ctx, cancel := context.WithCancel(ctx)
		s := &Server{ctx: ctx, cancel: cancel, workspace: &code.Workspace{RootPath: dir}, scope: workspaceScope(dir), backends: map[string]*backendEntry{}}
		s.ahpOnce.Do(func() { s.ahp = h.ahp.attach(s) })
		return s, nil
	}
	return h
}

func TestHostRejectsProjectsAfterClose(t *testing.T) {
	root := canonicalPath(t.TempDir())
	h := projectLifecycleHost(t, root)
	defer h.Close()
	if _, err := h.ahp.projectAt(root); err != nil {
		t.Fatal(err)
	}
	h.Close()
	h.ahp.open = func(string) (*Server, error) {
		t.Fatal("closed host started a project")
		return nil, nil
	}
	for _, dir := range []string{root, filepath.Join(root, "another")} {
		if _, err := h.ahp.projectAt(dir); !errors.Is(err, errHostClosed) {
			t.Fatalf("projectAt(%q) after close: %v", dir, err)
		}
	}
}

func TestHostCloseWaitsForOpeningProject(t *testing.T) {
	root := canonicalPath(t.TempDir())
	h := projectLifecycleHost(t, root)
	defer h.Close()
	open := h.ahp.open
	opening, release := make(chan *Server, 1), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	h.ahp.open = func(dir string) (*Server, error) {
		s, err := open(dir)
		opening <- s
		<-release
		return s, err
	}
	opened := make(chan error, 1)
	go func() { _, err := h.ahp.projectAt(root); opened <- err }()
	s := <-opening
	closed := make(chan struct{})
	go func() { h.Close(); close(closed) }()
	<-h.ctx.Done()
	select {
	case <-closed:
		t.Fatal("Close returned before the opening project's resources were closed")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := <-opened; !errors.Is(err, errHostClosed) {
		t.Fatalf("opening during close: %v", err)
	}
	<-closed
	if s.ctx.Err() == nil || len(h.ahp.loaded()) != 0 {
		t.Fatal("opening project survived host shutdown")
	}
}

func TestHostIdleProjectWaitsForTeardownBeforeReopening(t *testing.T) {
	root := canonicalPath(t.TempDir())
	h := projectLifecycleHost(t, root)
	defer h.Close()
	p, err := h.ahp.projectAt(root)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	p.server.background.Go(func() { <-release })
	idleClosed := make(chan struct{})
	go func() { h.ahp.closeIdle(0); close(idleClosed) }()
	<-p.done
	reopened := make(chan *ahpProject, 1)
	go func() {
		next, err := h.ahp.projectAt(root)
		if err != nil {
			t.Error(err)
		}
		reopened <- next
	}()
	select {
	case <-reopened:
		t.Fatal("project reopened while its previous instance was still closing")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	<-idleClosed
	if next := <-reopened; next == nil || next == p {
		t.Fatal("project did not reopen with a fresh instance")
	}
}

func TestProjectDetachForgetsLiveSessions(t *testing.T) {
	root := canonicalPath(t.TempDir())
	h := projectLifecycleHost(t, root)
	defer h.Close()
	p, err := h.ahp.projectAt(root)
	if err != nil {
		t.Fatal(err)
	}
	b := p.server.bindBackend("one", &protocolAgent{})
	p.server.backends[b.id] = &backendEntry{runtime: b}
	c := b.session("saved")
	main, err := p.attachSession(c, "ahp-session:/saved", "Saved", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.ahp.aliases[main.session.resource] = ahpAlias{Resource: main.session.resource, Project: p.dir, Backend: b.id, Session: c.ref.SessionID}
	p.server.Close()
	if len(h.ahp.loaded()) != 0 || len(h.ahp.sessions) != 0 || len(h.ahp.chats) != 0 {
		t.Fatal("closed project left live channels registered")
	}
	if len(h.ahp.aliases) != 1 {
		t.Fatal("closing the project forgot its saved session")
	}
	if _, err := p.attachSession(c, main.session.resource, "Saved", time.Now()); err == nil {
		t.Fatal("closed project reattached a session")
	}
}
