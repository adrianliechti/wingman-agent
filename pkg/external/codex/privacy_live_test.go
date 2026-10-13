package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Read the installed app-server's effective configuration without starting a
// thread or making an inference request. All settings and credentials are fake.
func TestLiveGatewayPrivacyConfig(t *testing.T) {
	if os.Getenv("CODEX_CONFIG_LIVE") != "1" {
		t.Skip("set CODEX_CONFIG_LIVE=1 to test the installed Codex CLI")
	}
	path, err := exec.LookPath(BinPath())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := `
model_provider = "openai"
cli_auth_credentials_store = "file"
web_search = "live"
[analytics]
enabled = true
[feedback]
enabled = true
[otel]
log_user_prompt = true
exporter = { otlp-http = { endpoint = "http://127.0.0.1:9/logs", protocol = "json" } }
trace_exporter = { otlp-http = { endpoint = "http://127.0.0.1:9/traces", protocol = "json" } }
metrics_exporter = "statsig"
[features]
apps = true
plugins = true
remote_plugin = true
ultrafast_mode = true
daemon_auto_start = true
realtime_conversation = true
shell_snapshot = true
[plugins."local@internal"]
enabled = true
[mcp_servers.internal]
command = "internal-mcp-fixture"
[model_providers.wingman]
name = "Old gateway"
base_url = "http://127.0.0.1:9/old"
env_key = "OLD_TOKEN"
requires_openai_auth = true
supports_websockets = true
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(dir, "auth.json")
	auth := []byte(`{"OPENAI_API_KEY":"old-saved-key"}`)
	if err := os.WriteFile(authPath, auth, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &CodexConfig{BaseURL: "http://127.0.0.1:9", AuthToken: "gateway-test-token", Models: []string{"gpt-6-astra"}}
	cleanup, err := PrepareModelCatalog(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, append(BuildArgs(cfg), "--strict-config", "app-server")...)
	cmd.Dir = dir
	cmd.Env = BuildEnv([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + dir,
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9",
	}, cfg)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = in.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("Codex exited: %v\n%s", err, stderr.String())
		}
	}()
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	call := func(id int, method string, params any) map[string]any {
		t.Helper()
		message := map[string]any{"id": id, "method": method, "params": params}
		if err := json.NewEncoder(in).Encode(message); err != nil {
			t.Fatal(err)
		}
		for scanner.Scan() {
			var response struct {
				ID     int             `json:"id"`
				Result map[string]any  `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &response) != nil || response.ID != id {
				continue
			}
			if len(response.Error) > 0 {
				t.Fatalf("%s: %s", method, response.Error)
			}
			return response.Result
		}
		t.Fatalf("no response to %s: %v", method, scanner.Err())
		return nil
	}
	call(1, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "wingman_privacy_test", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	})
	fmt.Fprintln(in, `{"method":"initialized"}`)
	result := call(2, "config/read", map[string]bool{"includeLayers": false})
	effective, ok := result["config"].(map[string]any)
	if !ok {
		t.Fatal("missing effective config")
	}
	check := func(want any, keys ...string) {
		t.Helper()
		var got any = effective
		for _, key := range keys {
			table, _ := got.(map[string]any)
			got = table[key]
		}
		if got != want {
			t.Errorf("%v = %v, want %v", keys, got, want)
		}
	}
	check("wingman", "model_provider")
	check("ephemeral", "cli_auth_credentials_store")
	check("disabled", "web_search")
	check(false, "analytics", "enabled")
	check(false, "feedback", "enabled")
	check(false, "otel", "log_user_prompt")
	for _, key := range []string{"exporter", "trace_exporter", "metrics_exporter"} {
		check("none", "otel", key)
	}
	for _, key := range []string{"apps", "remote_plugin", "plugin_sharing", "ultrafast_mode", "daemon_auto_start", "realtime_conversation", "shell_snapshot"} {
		check(false, "features", key)
	}
	check(true, "features", "plugins")
	check(true, "plugins", "local@internal", "enabled")
	check("internal-mcp-fixture", "mcp_servers", "internal", "command")
	check(cfg.BaseURL+"/v1", "model_providers", "wingman", "base_url")
	check("WINGMAN_TOKEN", "model_providers", "wingman", "env_key")
	check(false, "model_providers", "wingman", "requires_openai_auth")
	check(false, "shell_environment_policy", "ignore_default_excludes")
	check("", "shell_environment_policy", "set", "WINGMAN_TOKEN")
	account := call(3, "account/read", map[string]bool{"refreshToken": false})
	if account["account"] != nil {
		t.Error("gateway process loaded saved first-party authentication")
	}
	if remaining, err := os.ReadFile(authPath); err != nil || !bytes.Equal(remaining, auth) {
		t.Error("gateway launch changed the user's saved credentials")
	}
}
