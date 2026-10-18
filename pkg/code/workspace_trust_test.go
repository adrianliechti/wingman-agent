package code

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
)

type trustUI struct {
	calls int
	allow bool
}

func (u *trustUI) Confirm(context.Context, string) (bool, error) { u.calls++; return u.allow, nil }
func (*trustUI) Elicit(context.Context, tool.ElicitRequest) (tool.ElicitResult, error) {
	panic("unexpected elicitation")
}

func TestMCPTrustBindsToExactConfigurationAndWorkspace(t *testing.T) {
	testenv.WingmanHome(t)
	root := t.TempDir()
	manager := mcp.NewManager(&mcp.Config{Servers: map[string]mcp.ServerConfig{}})
	manager.TrustRequired = map[string]string{"local": filepath.Join(root, "mcp.json")}
	ws := &Workspace{RootPath: root, MCP: manager}
	config := mcp.ServerConfig{Command: "example", Args: []string{"serve"}, Env: map[string]string{"EXAMPLE": "one"}}
	if err := ws.approveMCP(t.Context(), nil, "local", config); err == nil {
		t.Fatal("startup trusted a repository configuration")
	}
	ui := &trustUI{allow: true}
	if err := ws.approveMCP(t.Context(), ui, "local", config); err != nil {
		t.Fatal(err)
	}
	if ui.calls != 1 {
		t.Fatalf("approval calls = %d", ui.calls)
	}
	ws = &Workspace{RootPath: root, MCP: manager}
	if err := ws.approveMCP(t.Context(), nil, "local", config); err != nil {
		t.Fatalf("approved config did not survive restart: %v", err)
	}
	changed := config
	changed.Args = []string{"different"}
	if err := ws.approveMCP(t.Context(), nil, "local", changed); err == nil {
		t.Fatal("changed command inherited approval")
	}
	changed = config
	changed.Env = map[string]string{"EXAMPLE": "two"}
	if err := ws.approveMCP(t.Context(), nil, "local", changed); err == nil {
		t.Fatal("changed environment inherited approval")
	}
	changed = config
	changed.Dir = t.TempDir()
	if err := ws.approveMCP(t.Context(), nil, "local", changed); err == nil {
		t.Fatal("changed working directory inherited approval")
	}
	other := &Workspace{RootPath: t.TempDir(), MCP: manager}
	if err := other.approveMCP(t.Context(), nil, "local", config); err == nil {
		t.Fatal("other workspace inherited approval")
	}
	info, err := os.Stat(filepath.Join(projectStateDir(root), "mcp-trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX permission bits; os.Stat always reports 0666 there.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatalf("approval file permissions = %v", info.Mode())
	}
}

func TestHookTrustPersistsPerWorkspace(t *testing.T) {
	testenv.WingmanHome(t)
	root := t.TempDir()
	if err := (&Workspace{RootPath: root}).HookTrust().Trust("sha256:a"); err != nil {
		t.Fatal(err)
	}
	restarted := (&Workspace{RootPath: root}).HookTrust()
	if !restarted.Trusted("sha256:a") || restarted.Trusted("sha256:b") {
		t.Fatal("hook approval did not survive restart for exactly the approved fingerprint")
	}
	if (&Workspace{RootPath: t.TempDir()}).HookTrust().Trusted("sha256:a") {
		t.Fatal("other workspace inherited hook approval")
	}
	path := filepath.Join(projectStateDir(root), "hook-trust.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if restarted.Trusted("sha256:a") {
		t.Fatal("unreadable trust file must not trust hooks")
	}
}

func TestCallMCPToolRequiresConnectedServer(t *testing.T) {
	ws := &Workspace{RootPath: t.TempDir(), MCP: mcp.NewManager(&mcp.Config{Servers: map[string]mcp.ServerConfig{}})}
	if _, err := ws.CallMCPTool(t.Context(), "policy", "check", nil); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("error = %v", err)
	}
	if _, err := (&Workspace{}).CallMCPTool(t.Context(), "policy", "check", nil); err == nil {
		t.Fatal("call without MCP succeeded")
	}
}

func TestProjectMCPIsBlockedBeforeStartingItsProcess(t *testing.T) {
	testenv.WingmanHome(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	// An invalid executable proves the manager returned at the trust boundary,
	// before attempting to launch even a misconfigured process.
	data, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"local": map[string]any{"command": marker}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	manager := loadMCP(dir, nil)
	if manager == nil || manager.TrustRequired["local"] == "" {
		t.Fatal("project provenance lost")
	}
	ws := &Workspace{RootPath: dir, MCP: manager}
	defer ws.Close()
	err = ws.InitMCP(t.Context())
	if err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("startup error = %v", err)
	}
	ui := &trustUI{}
	if err := ws.InitMCP(t.Context(), ui); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("denial error = %v", err)
	}
	if ui.calls != 1 {
		t.Fatalf("approval calls = %d", ui.calls)
	}
	if len(manager.Sessions()) != 0 {
		t.Fatal("unapproved server connected")
	}
}
