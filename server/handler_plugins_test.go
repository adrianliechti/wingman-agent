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

func TestHandlePluginsListsWhatEachPluginBrings(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)

	workDir := t.TempDir()
	plugin := filepath.Join(workDir, ".wingman", "plugins", "release-tools")
	if err := os.CopyFS(plugin, os.DirFS(filepath.Join("..", "examples", "plugins", "release-tools"))); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(workDir, ".wingman", "plugins", "bare")
	if err := os.MkdirAll(bare, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "bare"}`
	if err := os.WriteFile(filepath.Join(bare, "plugin.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	workspace, err := code.NewWorkspace(workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	server := &Server{Project: &Project{workspace: workspace}}
	response := httptest.NewRecorder()
	server.handlePlugins(response, httptest.NewRequest("GET", "/api/workspace/plugins", nil))
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var entries []PluginEntry
	if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	want := []PluginEntry{{Name: "bare", Scope: "project"}, {
		Name:        "release-tools",
		Version:     "1.0.0",
		Description: "Release checklist skill plus the MCP servers it needs",
		Scope:       "project",
		Skills:      1,
		MCPServers:  3,
		Hooks:       true,
	}}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %s", response.Body.String())
	}

	response = httptest.NewRecorder()
	server.handleSkills(response, httptest.NewRequest("GET", "/api/workspace/skills", nil))
	var skills []SkillEntry
	if err := json.Unmarshal(response.Body.Bytes(), &skills); err != nil {
		t.Fatal(err)
	}
	scopes := map[string]string{}
	for _, skill := range skills {
		scopes[skill.Name] = skill.Scope
	}
	if scopes["release-tools:release-check"] != "plugin" || scopes["commit"] != "builtin" {
		t.Fatalf("skill scopes = %v", scopes)
	}
}
