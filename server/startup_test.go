package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	codeagent "github.com/adrianliechti/wingman-agent/pkg/code/agent"
)

func TestListenLocalReservesDefaultAndFallbackPorts(t *testing.T) {
	first, err := listenLocal(0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := listenLocal(0)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.Addr().String() == second.Addr().String() {
		t.Fatalf("selected occupied address %s", first.Addr())
	}
	for _, listener := range []net.Listener{first, second} {
		port := listener.Addr().(*net.TCPAddr).Port
		if port == 0 {
			t.Fatal("listener has no assigned port")
		}
		duplicate, err := listenLocal(port)
		if err == nil {
			duplicate.Close()
			t.Fatalf("explicit occupied port %d silently fell back", port)
		}
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatalf("selected port is not listening: %v", err)
		}
		conn.Close()
	}
}

func TestModelDiscoveryFailureReachesLateWebClient(t *testing.T) {
	testenv.WingmanHome(t)
	var fail atomic.Bool
	fail.Store(true)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if fail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"test gateway authentication failed"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-6-sol"}]}`))
	}))
	defer gateway.Close()
	t.Setenv("WINGMAN_URL", gateway.URL)
	t.Setenv("WINGMAN_MODEL", "gpt-6-sol")
	app, err := New(t.Context(), t.TempDir(), &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	rt, err := app.backend("wingman")
	if err != nil {
		t.Fatal(err)
	}
	a := rt.agent.(*codeagent.Agent)
	deadline := time.After(5 * time.Second)
	for a.ModelDiscoveryError() == nil {
		select {
		case <-deadline:
			t.Fatal("model discovery did not report its failure")
		case <-time.After(10 * time.Millisecond):
		}
	}
	readSettings := func() SessionSettings {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://localhost/api/v2/backends/wingman/settings", nil)
		req.Header.Set(instanceHeader, app.scope.InstanceID)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("settings: %d %s", rec.Code, rec.Body)
		}
		var settings SessionSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
			t.Fatal(err)
		}
		return settings
	}
	settings := readSettings()
	if !strings.Contains(settings.ModelsError, "401 Unauthorized") || settings.Model != "gpt-6-sol" {
		t.Fatalf("late client settings: model %q, error %q", settings.Model, settings.ModelsError)
	}
	retry := func(wantStatus int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://localhost/api/v2/backends/wingman/models/refresh", nil)
		req.Header.Set(instanceHeader, app.scope.InstanceID)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("retry: %d %s, want %d", rec.Code, rec.Body, wantStatus)
		}
	}
	view := rt.session("discovery-test")
	retry(http.StatusBadGateway)
	fail.Store(false)
	retry(http.StatusNoContent)
	settings = readSettings()
	if settings.ModelsError != "" || len(settings.Models) != 1 || settings.Models[0].ID != "gpt-6-sol" {
		t.Fatalf("successful discovery did not clear failure: %#v", settings)
	}
	view.mu.Lock()
	liveSettings := view.state.Settings
	view.mu.Unlock()
	if liveSettings.ModelsError != "" || len(liveSettings.Models) != 1 {
		t.Fatalf("retry did not refresh the live session: %#v", liveSettings)
	}
	fail.Store(true)
	retry(http.StatusBadGateway)
	settings = readSettings()
	if settings.ModelsError == "" || len(settings.Models) != 1 || settings.Models[0].ID != "gpt-6-sol" {
		t.Fatalf("failed discovery discarded last known models: %#v", settings)
	}
}

type blockedDiscoveryAgent struct {
	*protocolAgent
	calls atomic.Int32
}

func (a *blockedDiscoveryAgent) FetchModels(ctx context.Context) error {
	a.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func TestModelDiscoverySharesConcurrentRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := &blockedDiscoveryAgent{protocolAgent: &protocolAgent{}}
		b := &backendRuntime{Server: &Server{ctx: ctx}, agent: a}
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- b.refreshModels() }()
		}
		synctest.Wait()
		if calls := a.calls.Load(); calls != 1 {
			t.Fatalf("concurrent retries started %d requests", calls)
		}
		cancel()
		for range 2 {
			if err := <-results; !errors.Is(err, context.Canceled) {
				t.Fatalf("shutdown did not cancel retry: %v", err)
			}
		}
		b.operations.Wait()
	})
}

func TestNativeAgentDoesNotRefreshGatewayModels(t *testing.T) {
	b := &backendRuntime{agent: &protocolAgent{}}
	rec := httptest.NewRecorder()
	b.handleRefreshModels(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("native refresh status = %d", rec.Code)
	}
}
