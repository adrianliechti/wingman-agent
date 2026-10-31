package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

func BinPath() string {
	if path := external.LookupPath("claude", ""); path != "" {
		return path
	}

	if path, err := FindPath(); err == nil {
		return path
	}

	return "claude"
}

func BuildVars(cfg *ClaudeConfig) map[string]string {
	vars := map[string]string{

		"ANTHROPIC_BASE_URL":       cfg.BaseURL,
		"ANTHROPIC_API_KEY":        "",
		"ANTHROPIC_AUTH_TOKEN":     cfg.AuthToken,
		"ANTHROPIC_CUSTOM_HEADERS": "",

		// Do not carry inherited subscription tokens into a gateway run.
		"CLAUDE_CODE_OAUTH_TOKEN":         "",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN": "",
		"CLAUDE_CODE_OAUTH_SCOPES":        "",

		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
		"CLAUDE_CODE_USE_BEDROCK":                    "",
		"CLAUDE_CODE_USE_VERTEX":                     "",
		"CLAUDE_CODE_USE_FOUNDRY":                    "",
		"CLAUDE_CODE_USE_MANTLE":                     "",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS":              "",
		"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD":     "",
		"CLAUDE_CODE_USE_GATEWAY":                    "",

		// The Default picker entry resolves through ANTHROPIC_DEFAULT_MODEL;
		// ANTHROPIC_MODEL would pin startup to it while Default resolved elsewhere.
		"ANTHROPIC_MODEL":         "",
		"ANTHROPIC_DEFAULT_MODEL": cfg.DefaultModel,

		// Background calls must also use a model the gateway serves.
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":        cmp.Or(cfg.HaikuModel, cfg.DefaultModel),
		"ANTHROPIC_DEFAULT_SONNET_MODEL":       cfg.SonnetModel,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":         cfg.OpusModel,
		"ANTHROPIC_DEFAULT_FABLE_MODEL":        cfg.FableModel,
		"ANTHROPIC_CUSTOM_MODEL_OPTION":        "",
		"CLAUDE_CODE_SUBAGENT_MODEL":           "inherit",
		"CLAUDE_CODE_SUBAGENT_MODEL_FORCE":     "0",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": "1",
	}
	maps.Copy(vars, privacyVars())
	return vars
}

// These values also go in session and parent settings: settings-file env
// entries otherwise overwrite the process environment, including opt-outs.
// Keep credentials out of this map because settings are passed on the argv.
func privacyVars() map[string]string {
	vars := map[string]string{
		"DISABLE_TELEMETRY":                           "1",
		"DISABLE_ERROR_REPORTING":                     "1",
		"DISABLE_GROWTHBOOK":                          "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":    "1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY":         "1",
		"CLAUDE_CODE_ENABLE_FEEDBACK_SURVEY_FOR_OTEL": "0",
		"CLAUDE_CODE_SEND_FEEDBACK":                   "0",
		"CLAUDE_CODE_ATTRIBUTION_HEADER":              "0",
		"CLAUDE_CODE_HIDE_CWD":                        "1",

		// Anthropic's telemetry opt-outs do not disable user-configured OTel.
		"CLAUDE_CODE_ENABLE_TELEMETRY":        "0",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "0",
		"ENABLE_ENHANCED_TELEMETRY_BETA":      "0",
		"ENABLE_BETA_TRACING_DETAILED":        "0",
		"BETA_TRACING_ENDPOINT":               "",
		"OTEL_METRICS_EXPORTER":               "none",
		"OTEL_LOGS_EXPORTER":                  "none",
		"OTEL_TRACES_EXPORTER":                "none",
		"OTEL_LOG_USER_PROMPTS":               "0",
		"OTEL_LOG_ASSISTANT_RESPONSES":        "0",
		"OTEL_LOG_TOOL_CONTENT":               "0",
		"OTEL_LOG_TOOL_DETAILS":               "0",
		"OTEL_LOG_RAW_API_BODIES":             "0",
		"OTEL_LOG_MANAGED_SETTINGS":           "0",
		"CLAUDE_CODE_PROPAGATE_TRACEPARENT":   "0",

		// Claude forces manual mode when the subprocess scrub is enabled.
		// Auto mode therefore allows child tools to inherit credentials.
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB": "0",
		// Keep auto-mode classifier calls on the regular gateway API path.
		"CLAUDE_CODE_AUTO_MODE_SERVER": "0",

		"DISABLE_AUTOUPDATER":                "1",
		"DISABLE_UPDATES":                    "1",
		"DISABLE_FEEDBACK_COMMAND":           "1",
		"DISABLE_INSTALLATION_CHECKS":        "1",
		"DISABLE_EXTRA_USAGE_COMMAND":        "1",
		"DISABLE_UPGRADE_COMMAND":            "1",
		"DISABLE_DOCTOR_COMMAND":             "1",
		"DISABLE_INSTALL_GITHUB_APP_COMMAND": "1",
		"DISABLE_LOGIN_COMMAND":              "1",
		"DISABLE_LOGOUT_COMMAND":             "1",

		"CLAUDE_CODE_DISABLE_FAST_MODE":          "1",
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",

		"DISABLE_COST_WARNINGS": "1",

		"CLAUDE_CODE_DISABLE_ARTIFACT": "1",
		// The shared supervisor can outlive and bypass this launch environment.
		"CLAUDE_CODE_DISABLE_AGENT_VIEW": "1",

		"IS_DEMO": "1",

		"CLAUDE_CODE_IDE_SKIP_AUTO_INSTALL": "1",

		"ENABLE_CLAUDEAI_MCP_SERVERS": "false",
		"CLAUDE_CODE_ENABLE_CFC":      "0",
		"CLAUDE_CODE_SYNC_SKILLS":     "0",

		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1",
	}
	maps.Copy(vars, hostVars())
	return vars
}

func BuildArgs(cfg *ClaudeConfig) []string {
	settings := map[string]any{
		"disableRemoteControl":          true,
		"disableWorkflows":              true,
		"ultracode":                     false,
		"workflowKeywordTriggerEnabled": false,
		"env":                           hostVars(),
	}
	if cfg != nil {
		maps.Copy(settings, map[string]any{
			"disableRemoteControl":      true,
			"disableClaudeAiConnectors": true,
			"syncClaudeAiSkills":        false,
			"syncClaudeAiPlugins":       false,
			"enableArtifact":            false,
			"disableAgentView":          true,
			"voice":                     map[string]bool{"enabled": false},
			"feedbackDrafts":            "off",
			"skipWebFetchPreflight":     true,
			"apiKeyHelper":              "",
			"otelHeadersHelper":         "",
			"env":                       privacyVars(),
		})
	}
	settingsJSON, _ := json.Marshal(settings)
	args := []string{"--settings", string(settingsJSON)}
	if cfg != nil {
		args = append(args, "--no-chrome")
		// The SDK's parent policy channel is required: --settings alone does
		// not enforce managed model restrictions. Never change machine policy.
		// "exact" also keeps Default on a listed model; enforceAvailableModels
		// would additionally ignore ANTHROPIC_DEFAULT_MODEL.
		settings["availableModels"] = cfg.Models
		settings["availableModelsMatch"] = "exact"
		settings["allowedProviders"] = []string{"customEndpoint"}
		// customEndpoint requires an exact managed endpoint pin. Only the URL,
		// never the authentication token, is included in the parent policy.
		settings["env"].(map[string]string)["ANTHROPIC_BASE_URL"] = cfg.BaseURL
		policy, _ := json.Marshal(settings)
		args = append(args, "--managed-settings", string(policy))
	}
	return args
}

func BuildEnv(parent []string, cfg *ClaudeConfig) []string {
	// Background tasks use ANTHROPIC_DEFAULT_HAIKU_MODEL instead of the
	// deprecated small/fast model override.
	return external.MergeEnv(parent, BuildVars(cfg), "ANTHROPIC_SMALL_FAST_MODEL")
}

// The host owns effort and task orchestration, including on the native backend.
func hostVars() map[string]string {
	return map[string]string{
		"CLAUDE_CODE_DISABLE_WORKFLOWS":  "1",
		"CLAUDE_CODE_DISABLE_ULTRATHINK": "1",
	}
}

func FindPath() (string, error) {
	if path, err := exec.LookPath("claude"); err == nil {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}

	candidates := []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".claude", "local", name),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("claude is not installed or not on PATH")
}

func Run(ctx context.Context, args []string, options *Options) error {
	options = external.WithDefaults(options)

	if options.Path == "" {
		options.Path = BinPath()
	}

	cfg, err := NewConfig(ctx, options)
	if err != nil {
		return err
	}

	return external.Run(ctx, options.Path, append(BuildArgs(cfg), args...), BuildEnv(options.Env, cfg))
}
