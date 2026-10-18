package code

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
)

type trustUI struct {
	calls int
	allow bool
}

type waitingTrustUI struct {
	entered chan struct{}
}

func (u waitingTrustUI) Confirm(ctx context.Context, _ string) (bool, error) {
	select {
	case u.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return false, ctx.Err()
}

func (waitingTrustUI) Elicit(context.Context, tool.ElicitRequest) (tool.ElicitResult, error) {
	panic("unexpected elicitation")
}

func TestMCPInitializationWaitIsCancellable(t *testing.T) {
	testenv.WingmanHome(t)
	manager := mcp.NewManager(&mcp.Config{Servers: map[string]mcp.ServerConfig{"local": {Command: "must-not-launch"}}})
	manager.TrustRequired = map[string]string{"local": "mcp.json"}
	ws := &Workspace{RootPath: t.TempDir(), MCP: manager}
	defer ws.Close()
	ui := waitingTrustUI{entered: make(chan struct{}, 1)}
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() { first <- ws.InitMCP(firstCtx, ui) }()
	defer func() { cancelFirst(); <-first }()
	select {
	case <-ui.entered:
	case <-time.After(time.Second):
		t.Fatal("first approval was not reached")
	}
	secondCtx, cancelSecond := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelSecond()
	second := make(chan error, 1)
	go func() { second <- ws.InitMCP(secondCtx, ui) }()
	select {
	case err := <-second:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancelled initialization = %v", err)
		}
	case <-time.After(time.Second):
		cancelFirst()
		<-second
		t.Fatal("cancelled initialization waited for another session's approval")
	}
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
	if err := ws.approveMCP(t.Context(), manager, nil, "local", config); err == nil {
		t.Fatal("startup trusted a repository configuration")
	}
	ui := &trustUI{allow: true}
	if err := ws.approveMCP(t.Context(), manager, ui, "local", config); err != nil {
		t.Fatal(err)
	}
	if ui.calls != 1 {
		t.Fatalf("approval calls = %d", ui.calls)
	}
	ws = &Workspace{RootPath: root, MCP: manager}
	if err := ws.approveMCP(t.Context(), manager, nil, "local", config); err != nil {
		t.Fatalf("approved config did not survive restart: %v", err)
	}
	changed := config
	changed.Args = []string{"different"}
	if err := ws.approveMCP(t.Context(), manager, nil, "local", changed); err == nil {
		t.Fatal("changed command inherited approval")
	}
	changed = config
	changed.Env = map[string]string{"EXAMPLE": "two"}
	if err := ws.approveMCP(t.Context(), manager, nil, "local", changed); err == nil {
		t.Fatal("changed environment inherited approval")
	}
	changed = config
	changed.Dir = t.TempDir()
	if err := ws.approveMCP(t.Context(), manager, nil, "local", changed); err == nil {
		t.Fatal("changed working directory inherited approval")
	}
	other := &Workspace{RootPath: t.TempDir(), MCP: manager}
	if err := other.approveMCP(t.Context(), manager, nil, "local", config); err == nil {
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

type closingTrustUI struct{ ws *Workspace }

func (u closingTrustUI) Confirm(context.Context, string) (bool, error) {
	u.ws.Close()
	return true, nil
}

func (closingTrustUI) Elicit(context.Context, tool.ElicitRequest) (tool.ElicitResult, error) {
	panic("unexpected elicitation")
}

func TestMCPInitializationRacingCloseLeavesNoSessions(t *testing.T) {
	testenv.WingmanHome(t)
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	httpServer := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil))
	defer httpServer.Close()
	headers := map[string]string{"Authorization": "Bearer test"}
	manager := mcp.NewManager(&mcp.Config{Servers: map[string]mcp.ServerConfig{
		"first":  {URL: httpServer.URL + "/first", Headers: headers},
		"second": {URL: httpServer.URL + "/second", Headers: headers},
	}})
	defer manager.Close()
	manager.TrustRequired = map[string]string{"first": "mcp.json", "second": "mcp.json"}
	ws := &Workspace{RootPath: t.TempDir(), MCP: manager}

	_ = ws.InitMCP(t.Context(), closingTrustUI{ws})

	sessions := manager.Sessions()
	if len(sessions) == 0 {
		t.Fatal("approved servers did not connect")
	}
	for name, session := range sessions {
		done := make(chan struct{})
		go func() { _ = session.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("MCP session %s outlived the closed workspace", name)
		}
	}
}

func TestLoadMCPKeepsProjectConfigWhenGlobalIsInvalid(t *testing.T) {
	testenv.WingmanHome(t)
	if err := os.MkdirAll(filepath.Dir(globalMCPConfigPath()), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalMCPConfigPath(), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"mcpServers":{"local":{"command":"example"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	manager := loadMCP(dir, nil)
	if manager == nil || manager.Servers["local"].Command != "example" || manager.TrustRequired["local"] == "" {
		t.Fatalf("project MCP config was dropped: %+v", manager)
	}
}
