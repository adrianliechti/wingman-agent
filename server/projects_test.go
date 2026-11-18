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
	h := newHost(t.Context())
	h.home = root
	h.open = func(dir string) (*Project, error) {
		ctx, cancel := context.WithCancel(h.ctx)
		s := &Project{ctx: ctx, cancel: cancel, workspace: &code.Workspace{RootPath: dir}, scope: workspaceScope(dir), backends: map[string]*backendEntry{}}
		h.attach(s)
		return s, nil
	}
	return h
}

func TestHostRejectsLookupsAfterShutdown(t *testing.T) {
	for _, shutdown := range []string{"cancel", "close"} {
		t.Run(shutdown, func(t *testing.T) {
			root := canonicalPath(t.TempDir())
			h := projectLifecycleHost(t, root)
			defer h.Close()
			if _, err := h.projectAt(root); err != nil {
				t.Fatal(err)
			}
			if shutdown == "cancel" {
				h.cancel()
			} else {
				h.Close()
			}
			h.open = func(string) (*Project, error) {
				t.Fatal("stopped host started a project")
				return nil, nil
			}
			for _, dir := range []string{root, filepath.Join(root, "another")} {
				if _, err := h.projectAt(dir); !errors.Is(err, errHostClosed) {
					t.Fatalf("projectAt(%q) after %s: %v", dir, shutdown, err)
				}
			}
			if _, _, err := h.ensureSession("ahp-session:/saved"); !errors.Is(err, errHostClosed) {
				t.Fatalf("session lookup after %s: %v", shutdown, err)
			}
		})
	}
}

func TestHostCloseWaitsForOpeningProject(t *testing.T) {
	root := canonicalPath(t.TempDir())
	h := projectLifecycleHost(t, root)
	defer h.Close()
	open := h.open
	opening, release := make(chan *Project, 1), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	h.open = func(dir string) (*Project, error) {
		s, err := open(dir)
		opening <- s
		<-release
		return s, err
	}
	opened := make(chan error, 1)
	go func() { _, err := h.projectAt(root); opened <- err }()
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
	if s.ctx.Err() == nil || len(h.loaded()) != 0 {
		t.Fatal("opening project survived host shutdown")
	}
}

func TestHostIdleProjectWaitsForTeardownBeforeReopening(t *testing.T) {
	root := canonicalPath(t.TempDir())
	h := projectLifecycleHost(t, root)
	defer h.Close()
	p, err := h.projectAt(root)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	p.background.Go(func() { <-release })
	idleClosed := make(chan struct{})
	go func() { h.closeIdle(0); close(idleClosed) }()
	<-p.ctx.Done()
	reopened := make(chan *Project, 1)
	go func() {
		next, err := h.projectAt(root)
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
	p, err := h.projectAt(root)
	if err != nil {
		t.Fatal(err)
	}
	b := p.bindBackend("one", &protocolAgent{})
	p.backends[b.id] = &backendEntry{runtime: b}
	c := b.session("saved")
	main, err := p.attachSession(c, "ahp-session:/saved", "Saved", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.aliases[main.session.resource] = sessionAlias{Resource: main.session.resource, Project: p.dir, Backend: b.id, Session: c.ref.SessionID}
	p.close()
	if len(h.loaded()) != 0 || len(h.sessions) != 0 || len(h.chats) != 0 {
		t.Fatal("closed project left live channels registered")
	}
	if len(h.aliases) != 1 {
		t.Fatal("closing the project forgot its saved session")
	}
	if _, err := p.attachSession(c, main.session.resource, "Saved", time.Now()); err == nil {
		t.Fatal("closed project reattached a session")
	}
}
