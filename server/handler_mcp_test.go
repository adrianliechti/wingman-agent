package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestHandleMCPServersListsStateAndTools(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)

	workDir := t.TempDir()
	config := `{"mcpServers": {"docs": {"url": "https://user:secret@mcp.example.com/mcp?key=secret", "headers": {"Authorization": "Bearer secret"}}}}`
	if err := os.WriteFile(filepath.Join(workDir, "mcp.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, err := code.NewWorkspace(workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	server := &Server{Project: &Project{workspace: workspace}}
	response := httptest.NewRecorder()
	server.handleMCPServers(response, httptest.NewRequest("GET", "/api/workspace/mcp", nil))
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var entries []MCPServerEntry
	if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	want := []MCPServerEntry{{Name: "docs", Transport: "http", Target: "https://mcp.example.com/mcp?[redacted]", Scope: "project", State: "pending", Tools: []MCPToolEntry{}}}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %s", response.Body.String())
	}

	empty, err := code.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	response = httptest.NewRecorder()
	(&Server{Project: &Project{workspace: empty}}).handleMCPServers(response, httptest.NewRequest("GET", "/api/workspace/mcp", nil))
	if body := response.Body.String(); body != "[]\n" {
		t.Fatalf("body without servers = %q", body)
	}
}
