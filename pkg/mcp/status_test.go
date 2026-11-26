package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestManagerStatusFollowsConnection(t *testing.T) {
	manager := NewManager(&Config{Servers: map[string]ServerConfig{
		"broken": {Command: filepath.Join(t.TempDir(), "missing")},
		"gated":  {Command: filepath.Join(t.TempDir(), "gated")},
	}})
	manager.TrustRequired = map[string]string{"gated": "mcp.json"}
	defer manager.Close()

	var changes atomic.Int32
	manager.SetStatusChangedHandler(func() { changes.Add(1) })

	for _, name := range []string{"broken", "gated", "unknown"} {
		if got := manager.Status(name); got != (Status{State: StatePending}) {
			t.Fatalf("%s before connecting = %+v", name, got)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Connect(ctx); err == nil {
		t.Fatal("connect succeeded")
	}

	if got := manager.Status("broken"); got.State != StateFailed || got.Error == "" || strings.HasPrefix(got.Error, "broken:") {
		t.Fatalf("broken = %+v", got)
	}
	if got := manager.Status("gated"); got.State != StateUnapproved || !strings.Contains(got.Error, "approval") {
		t.Fatalf("gated = %+v", got)
	}
	if changes.Load() < 3 {
		t.Fatalf("status changes = %d, want connecting, failed and unapproved", changes.Load())
	}

	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientSession, err := manager.newClient("live").Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.AddSession("live", clientSession)
	if got := manager.Status("live"); got.State != StateConnected {
		t.Fatalf("live = %+v", got)
	}

	if err := serverSession.Close(); err != nil {
		t.Fatal(err)
	}
	for manager.Status("live").State != StateDisconnected {
		select {
		case <-ctx.Done():
			t.Fatalf("live after the server left = %+v", manager.Status("live"))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestServerConfigTargetOmitsURLCredentials(t *testing.T) {
	tests := map[string]struct {
		server ServerConfig
		kind   string
		target string
	}{
		"stdio": {ServerConfig{Command: "npx", Args: []string{"-y", "server"}, Env: map[string]string{"TOKEN": "secret"}}, "stdio", "npx -y server"},
		"http":  {ServerConfig{URL: "https://user:secret@mcp.example.com/mcp?key=secret"}, "http", "https://mcp.example.com/mcp?[redacted]"},
		"sse":   {ServerConfig{URL: "https://mcp.example.com/sse", Transport: "sse"}, "sse", "https://mcp.example.com/sse"},
	}
	for name, test := range tests {
		if got := test.server.Kind(); got != test.kind {
			t.Errorf("%s kind = %q, want %q", name, got, test.kind)
		}
		if got := test.server.Target(); got != test.target {
			t.Errorf("%s target = %q, want %q", name, got, test.target)
		}
	}
}
