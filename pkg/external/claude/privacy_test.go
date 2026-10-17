package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestGatewayPrivacyOverridesInheritedSettings(t *testing.T) {
	cfg := &ClaudeConfig{
		BaseURL: "https://gateway.example.com", AuthToken: "private-gateway-token",
		Models: []string{"claude-sonnet-5"}, DefaultModel: "claude-sonnet-5",
	}
	parent := []string{
		"ANTHROPIC_API_KEY=old-key", "ANTHROPIC_AUTH_TOKEN=old-token",
		"ANTHROPIC_CUSTOM_HEADERS=Authorization: Bearer old-token",
		"CLAUDE_CODE_OAUTH_TOKEN=old-oauth", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN=old-refresh",
		"DISABLE_TELEMETRY=", "CLAUDE_CODE_ENABLE_TELEMETRY=1",
		"ENABLE_BETA_TRACING_DETAILED=1", "OTEL_LOG_RAW_API_BODIES=file:/tmp/raw",
		"OTEL_LOGS_EXPORTER=otlp", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1",
		"HTTPS_PROXY=http://enterprise-proxy:8080", "NODE_EXTRA_CA_CERTS=/etc/corp.pem",
	}
	env := BuildEnv(parent, cfg)
	for _, stale := range parent[:11] {
		if slices.Contains(env, stale) {
			t.Errorf("inherited override survived: %q", stale)
		}
	}
	for _, keep := range parent[11:] {
		if !slices.Contains(env, keep) {
			t.Errorf("enterprise network configuration removed: %q", keep)
		}
	}

	args := BuildArgs(cfg)
	if strings.Contains(strings.Join(args, " "), cfg.AuthToken) {
		t.Fatal("gateway credential exposed in process arguments")
	}
	for _, flag := range []string{"--settings", "--managed-settings"} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("missing %s", flag)
		}
		var settings struct {
			Env                       map[string]string `json:"env"`
			SkipWebFetchPreflight     bool              `json:"skipWebFetchPreflight"`
			DisableRemoteControl      bool              `json:"disableRemoteControl"`
			DisableClaudeAiConnectors bool              `json:"disableClaudeAiConnectors"`
			DisableAgentView          bool              `json:"disableAgentView"`
			SyncClaudeAiSkills        *bool             `json:"syncClaudeAiSkills"`
			SyncClaudeAiPlugins       *bool             `json:"syncClaudeAiPlugins"`
			AllowedProviders          []string          `json:"allowedProviders"`
		}
		if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"CLAUDE_CODE_ENABLE_TELEMETRY": "0", "OTEL_LOGS_EXPORTER": "none",
			"OTEL_LOG_RAW_API_BODIES": "0", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB": "0",
			"DISABLE_TELEMETRY": "1", "DISABLE_UPDATES": "1", "CLAUDE_CODE_AUTO_MODE_SERVER": "0",
		} {
			if settings.Env[key] != want || !slices.Contains(env, key+"="+want) {
				t.Errorf("%s must pin %s=%s in settings and process environment", flag, key, want)
			}
		}
		if !settings.SkipWebFetchPreflight || !settings.DisableRemoteControl || !settings.DisableClaudeAiConnectors || !settings.DisableAgentView {
			t.Errorf("%s leaves an external service or unwrapped supervisor enabled", flag)
		}
		if settings.SyncClaudeAiSkills == nil || *settings.SyncClaudeAiSkills || settings.SyncClaudeAiPlugins == nil || *settings.SyncClaudeAiPlugins {
			t.Errorf("%s leaves cloud account sync enabled", flag)
		}
		if flag == "--managed-settings" && (!slices.Equal(settings.AllowedProviders, []string{"customEndpoint"}) || settings.Env["ANTHROPIC_BASE_URL"] != cfg.BaseURL) {
			t.Error("parent policy must restrict model routing to the pinned gateway")
		}
	}
	for _, forbidden := range []string{"--bare", "--safe-mode", "--strict-mcp-config", "--setting-sources", "--disable-slash-commands"} {
		if slices.Contains(args, forbidden) {
			t.Errorf("%s prevents existing local integrations from loading", forbidden)
		}
	}
}
