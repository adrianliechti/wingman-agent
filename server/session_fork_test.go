package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/go-chi/chi/v5"
)

type forkingProtocolAgent struct {
	*protocolAgent
	forks       atomic.Int32
	source      string
	title       string
	busySources map[string]bool
}

func (a *forkingProtocolAgent) ForkSession(_ context.Context, id, title string) (string, error) {
	if a.busySources[id] {
		return "", code.ErrTurnInProgress
	}
	a.forks.Add(1)
	a.source, a.title = id, title
	return "forked-" + id, nil
}

func newForkProtocolServer(t *testing.T, a code.Agent) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s := &Server{Project: &Project{ctx: ctx, cancel: cancel, scope: workspaceScope(protocolProject), backends: map[string]*backendEntry{}}}
	s.Project = webProject(s, protocolProject)
	b := s.Project.bindBackend("one", a)
	s.Project.backends[b.id] = &backendEntry{runtime: b}
	s.mux = chi.NewRouter()
	s.mux.Route("/api", s.registerSessionRoutes)
	s.handler = s.checkInstance(s.mux)
	t.Cleanup(s.Close)
	return s
}

func TestForkSessionCreates(t *testing.T) {
	a := &forkingProtocolAgent{protocolAgent: &protocolAgent{}}
	s := newForkProtocolServer(t, a)

	command := Command{ID: "fork-request", Type: "fork", Source: "saved", Title: "Second try"}
	var receipt Receipt
	rec := protocolRequest(t, s, "/api/v2/backends/one/sessions", command)
	if rec.Code != 200 {
		t.Fatalf("fork: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if a.forks.Load() != 1 || a.source != "saved" || a.title != "Second try" {
		t.Fatalf("forks=%d source=%q title=%q", a.forks.Load(), a.source, a.title)
	}
	if receipt.Ref.SessionID != "forked-saved" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if !s.Project.backends["one"].runtime.settings("").CanFork {
		t.Fatal("settings do not advertise forking")
	}
}

func TestForkSessionRejectsBusyMissingAndUnsupported(t *testing.T) {
	a := &forkingProtocolAgent{protocolAgent: &protocolAgent{}, busySources: map[string]bool{"busy": true}}
	s := newForkProtocolServer(t, a)
	b := s.Project.backends["one"].runtime
	if result := b.createSession(Command{ID: "busy-fork", Type: "fork", Source: "busy"}); !errors.Is(result.err, code.ErrTurnInProgress) {
		t.Fatalf("busy fork: %v", result.err)
	}
	if result := b.createSession(Command{ID: "no-source", Type: "fork"}); result.err == nil || !strings.Contains(result.err.Error(), "source session") {
		t.Fatalf("fork without source: %v", result.err)
	}

	plain := newForkProtocolServer(t, &protocolAgent{})
	if result := plain.Project.backends["one"].runtime.createSession(Command{ID: "unsupported", Type: "fork", Source: "saved"}); result.err == nil || !strings.Contains(result.err.Error(), "supports forking") {
		t.Fatalf("fork on a non-forking agent: %v", result.err)
	}
	if plain.Project.backends["one"].runtime.settings("").CanFork {
		t.Fatal("non-forking agent advertises forking")
	}
}
