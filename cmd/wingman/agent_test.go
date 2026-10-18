package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

func TestAgentHostOwnsWorkspaceAndTelemetry(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") {
			t.Setenv(name, "")
		}
	}
	var root atomic.Pointer[os.Root]
	var exports atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if _, err := root.Load().Stat("."); !errors.Is(err, os.ErrClosed) {
			t.Errorf("telemetry exported before the workspace closed: %v", err)
		}
		exports.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", collector.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	t.Setenv("WINGMAN_OUTPUT_TOKEN_BUDGET", "")
	t.Setenv("WINGMAN_TASK_MAX_TOKENS", "")
	t.Setenv("WINGMAN_TASK_TIMEOUT", "")

	host, err := newAgentHost(t.Context(), t.TempDir(), code.BuiltinAgentName)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	root.Store(host.workspace.Root)
	if host.telemetry == nil {
		t.Fatal("host did not initialize telemetry")
	}
	// Closing a borrower leaves host resources available for other consumers.
	if err := host.agent.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := host.workspace.Root.Stat("."); err != nil {
		t.Fatalf("agent closed its borrowed workspace: %v", err)
	}
	_, invocation := host.telemetry.StartAgent(t.Context(), telemetry.AgentRequest{Name: "after-agent-close"})
	invocation.End(telemetry.Outcome{})

	closeErr := errors.New("agent close failed")
	closes := 0
	host.agent = &closeTestAgent{Agent: host.agent, close: func() error {
		closes++
		if _, err := host.workspace.Root.Stat("."); err != nil {
			t.Errorf("workspace closed before its agent: %v", err)
		}
		return closeErr
	}}
	for range 2 {
		if err := host.Close(); !errors.Is(err, closeErr) {
			t.Fatalf("Close() = %v, want agent close error", err)
		}
	}
	if closes != 1 || exports.Load() != 1 {
		t.Fatalf("agent closes=%d telemetry exports=%d, want one each", closes, exports.Load())
	}
	if _, err := host.workspace.Root.Stat("."); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("host left workspace open: %v", err)
	}
}

func TestExternalAgentStartupDoesNotLoadBuiltinConfig(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	t.Setenv("WINGMAN_OUTPUT_TOKEN_BUDGET", "invalid")
	_, err := newAgentHost(context.Background(), t.TempDir(), "missing-agent-for-test")
	if err == nil || !strings.Contains(err.Error(), `unknown agent "missing-agent-for-test"`) {
		t.Fatalf("external startup unexpectedly loaded built-in config: %v", err)
	}
}

type closeTestAgent struct {
	code.Agent
	close func() error
}

func (a *closeTestAgent) Close() error { return a.close() }
