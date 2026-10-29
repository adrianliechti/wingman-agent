package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
)

// Exercise the real browser facade and official TypeScript client against the
// Go host. The provider is deterministic; transport, controller, queue, prompts,
// filesystem and PTYs are real. No model credentials or VS Code are required.
func TestAHPTypeScriptIntegration(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for TypeScript integration tests")
	}
	if _, err := os.Stat("ui/node_modules/@microsoft/agent-host-protocol/package.json"); err != nil {
		t.Skip("run npm ci in server/ui to install the official TypeScript client")
	}
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, b, agent := newProtocolServer(t)
	b.agent = &completionProtocolAgent{agent}
	root := t.TempDir()
	for path, content := range map[string]string{
		".agents/skills/shared/SKILL.md":                  "---\nname: shared\ndescription: Project shared workflow\n---\nPROJECT WORKFLOW $ARGUMENTS",
		".agents/skills/native/SKILL.md":                  "---\nname: native\ndescription: Workspace skill with a native command name\n---\nWORKSPACE NATIVE SKILL",
		".wingman/plugins/reports/plugin.json":            `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"reports"}`,
		".wingman/plugins/reports/skills/shared/SKILL.md": "---\nname: shared\ndescription: Plugin shared workflow\n---\nPLUGIN WORKFLOW $ARGUMENTS",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "AHP test"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %v: %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-qm", "fixture"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %v: %s", err, out)
		}
	}
	workspace, err := code.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	s.workspace = workspace
	workspace.WarmUp()
	s.terminals = terminal.NewManager(workspace.RootPath)
	s.ahpHost()
	agent.gate = make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(agent.gate) }) })
	s.mux.Get("/ahp", s.handleAHP)
	s.mux.Get("/api/workspace", s.handleWorkspace)
	s.mux.Get("/api/workspace/skills", s.handleSkills)
	s.mux.Get("/fixture/inputs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, agent.Messages("")) })
	s.mux.Get("/api/workspace/git/diff", s.handleGitDiff)
	s.mux.Get("/api/workspace/git", s.handleWorkspaceGit)
	s.mux.Post("/fixture/release", func(w http.ResponseWriter, r *http.Request) {
		release.Do(func() { close(agent.gate) })
		w.WriteHeader(204)
	})
	s.mux.Post("/fixture/confirm", func(w http.ResponseWriter, r *http.Request) {
		accepted, err := b.Confirm(code.WithSessionID(r.Context(), r.URL.Query().Get("session")), "Allow test operation?")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"accepted": accepted})
	})
	s.mux.Post("/fixture/input", func(w http.ResponseWriter, r *http.Request) {
		result, err := b.Elicit(code.WithSessionID(r.Context(), r.URL.Query().Get("session")), tool.ElicitRequest{Message: "Choose one", Fields: []tool.ElicitField{{Name: "choice", Type: "string", Enum: []string{"one", "two"}}}})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, result)
	})
	server := httptest.NewServer(s)
	defer server.Close()
	scope, _ := json.Marshal(map[string]any{"workspaceId": s.scope.WorkspaceID, "instanceId": s.scope.InstanceID, "protocol": 2, "backends": []map[string]string{{"id": "one", "name": "Test"}}})
	cmd := exec.CommandContext(t.Context(), node, "--test", "test/ahp.integration.ts")
	cmd.Dir = "ui"
	cmd.Env = append(os.Environ(), "AHP_TEST_URL=ws"+strings.TrimPrefix(server.URL, "http")+"/ahp", "AHP_TEST_HTTP="+server.URL, "AHP_TEST_SCOPE="+string(scope), "AHP_TEST_ROOT="+filepath.Clean(workspace.RootPath))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TypeScript client integration: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}

type completionProtocolAgent struct{ *protocolAgent }

func (a *completionProtocolAgent) Commands(sessionID string) []code.Command {
	return []code.Command{
		{Name: "/native", Description: "A native backend command", InputHint: "[target]"},
		{Name: "only-" + sessionID, Description: "Only this session"},
	}
}
