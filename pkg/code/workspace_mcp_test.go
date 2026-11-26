package code

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
)

func TestWorkspaceMCPServersReportStateScopeAndTools(t *testing.T) {
	testenv.WingmanHome(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")

	writeFile := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(globalMCPConfigPath(), `{"mcpServers": {
		"broken": {"command": "`+filepath.ToSlash(missing)+`", "args": ["--stdio"], "env": {"TOKEN": "secret"}},
		"remote": {"url": "http://user:secret@127.0.0.1:1/mcp?key=secret", "transport": "sse", "headers": {"Authorization": "Bearer secret"}}
	}}`)
	writeFile(filepath.Join(dir, "mcp.json"), `{"mcpServers": {"local": {"command": "`+filepath.ToSlash(missing)+`"}}}`)

	manager := loadMCP(dir, nil)
	if manager == nil {
		t.Fatal("no MCP manager")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "lookup", Description: "Look something up"},
		func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{}, nil, nil
		})
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}

	w := &Workspace{RootPath: dir, MCP: manager}
	defer w.Close()
	var changes atomic.Int32
	w.SetMCPChangedHandler(func() { changes.Add(1) })

	for _, server := range w.MCPServers() {
		if server.State != mcp.StatePending || len(server.Tools) != 0 {
			t.Fatalf("%s before start = %+v", server.Name, server)
		}
	}

	manager.AddSession("live", clientSession)
	if err := w.InitMCP(ctx); err == nil {
		t.Fatal("broken servers started")
	}
	if changes.Load() == 0 {
		t.Fatal("no change was reported")
	}

	servers := map[string]MCPServer{}
	var order []string
	for _, server := range w.MCPServers() {
		servers[server.Name] = server
		order = append(order, server.Name)
	}
	if want := []string{"broken", "live", "local", "remote"}; !slices.Equal(order, want) {
		t.Fatalf("servers = %v, want %v", order, want)
	}

	broken := servers["broken"]
	if broken.State != mcp.StateFailed || broken.Error == "" || broken.Scope != "global" || broken.Transport != "stdio" || broken.Target != filepath.ToSlash(missing)+" --stdio" {
		t.Errorf("broken = %+v", broken)
	}
	local := servers["local"]
	if local.State != mcp.StateUnapproved || local.Scope != "project" {
		t.Errorf("local = %+v", local)
	}
	remote := servers["remote"]
	if remote.State != mcp.StateFailed || remote.Transport != "sse" || remote.Target != "http://127.0.0.1:1/mcp?[redacted]" || strings.Contains(remote.Error, "secret") {
		t.Errorf("remote = %+v", remote)
	}
	live := servers["live"]
	if live.State != mcp.StateConnected || len(live.Tools) != 1 || live.Tools[0] != (MCPTool{Name: "lookup", Description: "Look something up"}) {
		t.Errorf("live = %+v", live)
	}

	if (&Workspace{}).MCPServers() != nil {
		t.Error("workspace without MCP lists servers")
	}
}
