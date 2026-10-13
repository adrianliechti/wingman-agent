package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the installed CLI with synthetic data, isolated settings, and a
// loopback gateway/proxy. No enterprise credentials or real inference are used.
func TestLiveGatewayPrivacyAndAutoMode(t *testing.T) {
	if os.Getenv("CLAUDE_PRIVACY_LIVE") != "1" {
		t.Skip("set CLAUDE_PRIVACY_LIVE=1 to test the installed Claude CLI")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's local hook uses POSIX shell quoting")
	}
	path, err := exec.LookPath(BinPath())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var requests atomic.Int32
	var gateway *httptest.Server
	gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != strings.TrimPrefix(gateway.URL, "http://") {
			t.Errorf("CLI attempted a request outside the gateway: %s %s", r.Method, r.Host)
			http.Error(w, "external traffic blocked", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/hello":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-5"}]}`)
		case "/v1/messages":
			requests.Add(1)
			if r.Header.Get("Authorization") != "Bearer test-gateway-token" || r.Header.Get("X-Api-Key") != "" {
				t.Error("model request did not use exclusively the gateway credential")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []string{
				`{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"READY"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			} {
				var eventType struct{ Type string }
				_ = json.Unmarshal([]byte(event), &eventType)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType.Type, event)
			}
		default:
			t.Errorf("unexpected gateway request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer gateway.Close()

	record := filepath.Join(dir, "hook.json")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	settings := map[string]any{
		"env": map[string]string{
			"DISABLE_TELEMETRY": "", "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
			"OTEL_LOGS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": gateway.URL + "/telemetry",
			"OTEL_LOG_RAW_API_BODIES": "1", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB": "1",
			"ANTHROPIC_BASE_URL": "https://wrong-gateway.invalid", "ANTHROPIC_API_KEY": "wrong-key",
		},
		"hooks": map[string]any{"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{
			"type":    "command",
			"command": "env WINGMAN_CLAUDE_PRIVACY_HOOK=1 " + quote(testBinary) + " -test.run=^TestClaudePrivacyHookHelper$ -- " + quote(record),
		}}}}},
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &ClaudeConfig{
		BaseURL: gateway.URL, AuthToken: "test-gateway-token", Models: []string{"claude-sonnet-5"},
		DefaultModel: "claude-sonnet-5", SonnetModel: "claude-sonnet-5", HaikuModel: "claude-sonnet-5",
	}
	args := append(BuildArgs(cfg), "--permission-mode", "auto", "--print", "--output-format", "json", "Reply READY without using tools.")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = BuildEnv([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CLAUDE_CONFIG_DIR=" + dir,
		"TMPDIR=" + dir, "HTTP_PROXY=" + gateway.URL, "HTTPS_PROXY=" + gateway.URL,
		"NO_PROXY=localhost,127.0.0.1", "CLAUDE_CODE_SKIP_PROMPT_HISTORY=1",
	}, cfg)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("Claude failed: %v\n%s", err, output.String())
	}
	if requests.Load() == 0 {
		t.Fatalf("Claude sent no model request to the test gateway: %s", output.String())
	}
	data, err = os.ReadFile(record)
	if err != nil {
		t.Fatalf("local hook did not run: %v\n%s", err, output.String())
	}
	var observed struct {
		Mode string            `json:"mode"`
		Env  map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Mode != "auto" {
		t.Errorf("actual CLI permission mode = %q, want auto\n%s", observed.Mode, output.String())
	}
	for key, want := range map[string]string{
		"DISABLE_TELEMETRY": "1", "CLAUDE_CODE_ENABLE_TELEMETRY": "0",
		"OTEL_LOGS_EXPORTER": "none", "OTEL_LOG_RAW_API_BODIES": "0",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB": "0", "ANTHROPIC_BASE_URL": gateway.URL,
	} {
		// Claude also removes OTel variables from hook subprocesses on some
		// versions. An absent variable keeps collection off in the child.
		if strings.HasPrefix(key, "OTEL_") && observed.Env[key] == "" {
			continue
		}
		if observed.Env[key] != want {
			t.Errorf("effective %s = %q, want %q", key, observed.Env[key], want)
		}
	}

	t.Run("reject endpoint outside parent policy", func(t *testing.T) {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Dir = dir
		cmd.Env = BuildEnv([]string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CLAUDE_CONFIG_DIR=" + dir,
			"TMPDIR=" + dir, "HTTP_PROXY=" + gateway.URL, "HTTPS_PROXY=" + gateway.URL,
			"NO_PROXY=localhost,127.0.0.1",
		}, cfg)
		for i, entry := range cmd.Env {
			if strings.HasPrefix(entry, "ANTHROPIC_BASE_URL=") {
				cmd.Env[i] = "ANTHROPIC_BASE_URL=" + gateway.URL + "/unapproved"
			}
		}
		output, err := cmd.CombinedOutput()
		if err == nil || !bytes.Contains(output, []byte("managed settings")) {
			t.Fatalf("unpinned endpoint was not refused by provider policy: %v\n%s", err, output)
		}
	})
}

func TestClaudePrivacyHookHelper(t *testing.T) {
	if os.Getenv("WINGMAN_CLAUDE_PRIVACY_HOOK") != "1" {
		t.Skip("local hook fixture")
	}
	var input struct {
		Mode string `json:"permission_mode"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		os.Exit(1)
	}
	env := make(map[string]string)
	for _, key := range []string{"DISABLE_TELEMETRY", "CLAUDE_CODE_ENABLE_TELEMETRY", "OTEL_LOGS_EXPORTER", "OTEL_LOG_RAW_API_BODIES", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB", "ANTHROPIC_BASE_URL"} {
		env[key] = os.Getenv(key)
	}
	data, _ := json.Marshal(map[string]any{"mode": input.Mode, "env": env})
	if len(flag.Args()) != 1 || os.WriteFile(flag.Args()[0], data, 0o600) != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
