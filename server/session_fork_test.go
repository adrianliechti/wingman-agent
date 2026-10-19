package server

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/coder/websocket"
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
	s := &Server{ctx: ctx, cancel: cancel, scope: WorkspaceScope{"workspace", "instance"}, backends: map[string]*backendEntry{}, wsConns: map[*websocket.Conn]*wsClient{}}
	b := s.bindBackend("one", a)
	s.backends[b.id] = &backendEntry{runtime: b}
	s.mux = chi.NewRouter()
	s.mux.Route("/api", s.registerSessionRoutes)
	s.handler = s.checkInstance(s.mux)
	t.Cleanup(s.Close)
	return s
}

func TestForkSessionCreatesAndDeduplicates(t *testing.T) {
	a := &forkingProtocolAgent{protocolAgent: &protocolAgent{}}
	s := newForkProtocolServer(t, a)

	command := Command{ID: "fork-request", Type: "fork", Source: "saved", Title: "Second try"}
	var receipt Receipt
	for range 2 {
		rec := protocolRequest(t, s, "/api/v2/backends/one/sessions", command)
		if rec.Code != 200 {
			t.Fatalf("fork: %d %s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
	}
	if a.forks.Load() != 1 || a.source != "saved" || a.title != "Second try" {
		t.Fatalf("forks=%d source=%q title=%q", a.forks.Load(), a.source, a.title)
	}
	if receipt.Ref.SessionID != "forked-saved" || receipt.Outcome != "created" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if !s.backends["one"].runtime.settings("").CanFork {
		t.Fatal("settings do not advertise forking")
	}
}

func TestForkSessionRejectsBusyMissingAndUnsupported(t *testing.T) {
	a := &forkingProtocolAgent{protocolAgent: &protocolAgent{}, busySources: map[string]bool{"busy": true}}
	s := newForkProtocolServer(t, a)
	if rec := protocolRequest(t, s, "/api/v2/backends/one/sessions", Command{ID: "busy-fork", Type: "fork", Source: "busy"}); rec.Code != 409 {
		t.Fatalf("busy fork: %d %s", rec.Code, rec.Body)
	}
	if rec := protocolRequest(t, s, "/api/v2/backends/one/sessions", Command{ID: "no-source", Type: "fork"}); rec.Code != 400 {
		t.Fatalf("fork without source: %d %s", rec.Code, rec.Body)
	}

	plain := newForkProtocolServer(t, &protocolAgent{})
	if rec := protocolRequest(t, plain, "/api/v2/backends/one/sessions", Command{ID: "unsupported", Type: "fork", Source: "saved"}); rec.Code != 400 {
		t.Fatalf("fork on a non-forking agent: %d %s", rec.Code, rec.Body)
	}
	if plain.backends["one"].runtime.settings("").CanFork {
		t.Fatal("non-forking agent advertises forking")
	}
}
