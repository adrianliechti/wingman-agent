package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	claudecli "github.com/adrianliechti/wingman-agent/pkg/external/claude"
)

// This check initializes the installed CLI against a loopback gateway. It
// submits no prompt and rejects all inference requests.
func TestLiveGatewayModelDiscovery(t *testing.T) {
	if os.Getenv("CLAUDE_DISCOVERY_LIVE") != "1" {
		t.Skip("set CLAUDE_DISCOVERY_LIVE=1 to test the installed Claude CLI")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"claude-sonnet-5", "claude-haiku-4-5", "anthropic/claude-sonnet-4-6", "internal/claude-future-99"}
	var discoveries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/api/hello":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			if r.URL.Query().Get("limit") == "1000" {
				discoveries.Add(1)
			}
			var data []map[string]string
			for _, id := range ids {
				data = append(data, map[string]string{"id": id})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		default:
			t.Errorf("unexpected gateway request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "no inference allowed", http.StatusForbidden)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg, err := claudecli.NewConfig(ctx, &claudecli.Options{WingmanURL: server.URL, WingmanToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := append(claudecli.BuildArgs(cfg), "--setting-sources", "")
	a := New(Options{
		Path: path, Args: args, Cwd: dir, Stderr: io.Discard,
		Env: claudecli.BuildEnv([]string{"PATH=" + os.Getenv("PATH"), "CLAUDE_CONFIG_DIR=" + dir}, cfg),
	})
	models, _, err := a.fetchModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if discoveries.Load() == 0 {
		t.Fatal("Claude did not perform native gateway discovery")
	}
	for _, m := range models {
		if !slices.Contains(ids, m.ResolvedModel) {
			t.Errorf("picker contains an unavailable model: %+v", m)
		}
	}
	for _, id := range ids {
		if resolveModel(models, id) == nil {
			t.Errorf("gateway model %q missing from picker: %+v", id, models)
		}
	}
	if m := findModel(models, "default"); m == nil || m.ResolvedModel != cfg.DefaultModel {
		t.Fatalf("default model = %+v, want %s", m, cfg.DefaultModel)
	}
	s := a.newSession("gateway-test", dir, "default", "default", nil)
	if !slices.Equal(s.cliArgsLocked()[:len(args)], args) {
		t.Fatal("session process does not receive the discovery process's model policy")
	}
}
