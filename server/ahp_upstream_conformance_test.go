package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Run Microsoft's black-box suite against an isolated multi-project host:
// WINGMAN_AHP_CONFORMANCE=/path/to/agent-host-protocol-contrib/tools/ahp-conformance/src/cli.ts go test ./server -run '^TestAHPUpstreamConformance$' -v
// Install the suite's dependencies first. Model-backed tests are disabled; the
// existing TypeScript integration covers turns against a deterministic agent.
func TestAHPUpstreamConformance(t *testing.T) {
	cli := os.Getenv("WINGMAN_AHP_CONFORMANCE")
	if cli == "" {
		t.Skip("set WINGMAN_AHP_CONFORMANCE to Microsoft's installed conformance CLI")
	}
	if _, err := os.Stat(cli); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node 24+ is required for upstream conformance")
	}
	testenv.WingmanHome(t)
	ahpTestGateway(t)
	root := canonicalPath(t.TempDir())
	host, err := NewHost(t.Context(), &HostOptions{Roots: []string{root}, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	server := httptest.NewServer(host)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ahp"
	// A host grants writes, watches and terminals to projects used by a session.
	// Provision the shared scratch tree so independent resource tests can run.
	client := ahpTestClient(t, endpoint, "conformance-fixture")
	if err := client.Request(t.Context(), "createSession", types.CreateSessionParams{
		Channel: "ahp-session:/conformance-workspace", Provider: ahpPtr("wingman"),
		WorkingDirectories: []string{fileuri.FromPath(root)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	report := os.Getenv("WINGMAN_AHP_CONFORMANCE_REPORT")
	if report == "" {
		report = filepath.Join(t.TempDir(), "conformance.json")
	}
	config := map[string]any{
		"target":    map[string]any{"url": endpoint},
		"agent":     map[string]any{"provider": "wingman"},
		"model":     map[string]any{"enabled": false},
		"workspace": map[string]any{"scratchDir": root, "sharedFilesystem": true},
		"run":       map[string]any{"concurrency": 4, "timeoutMs": 10000},
		"report":    map[string]any{"reporter": "cli", "open": false, "jsonFile": report},
	}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(t.TempDir(), "conformance.json")
	if err := os.WriteFile(configFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, cli, configFile, "--reporter", "cli", "--no-open")
	output, err := cmd.CombinedOutput()
	t.Logf("Upstream conformance report: %s\n%s", report, output)
	if err != nil {
		t.Fatalf("upstream conformance failed: %v", err)
	}
}
