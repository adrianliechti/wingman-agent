package claude

import (
	"context"
	"encoding/json"
	"fmt"
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

		"ANTHROPIC_BASE_URL":   cfg.BaseURL,
		"ANTHROPIC_API_KEY":    "",
		"ANTHROPIC_AUTH_TOKEN": cfg.AuthToken,

		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
		"CLAUDE_CODE_USE_BEDROCK":                    "",
		"CLAUDE_CODE_USE_VERTEX":                     "",
		"CLAUDE_CODE_USE_FOUNDRY":                    "",
		"CLAUDE_CODE_USE_MANTLE":                     "",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS":              "",
		"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD":     "",
		"CLAUDE_CODE_USE_GATEWAY":                    "",

		"ANTHROPIC_MODEL":                  cfg.DefaultModel,
		"ANTHROPIC_DEFAULT_MODEL":          cfg.DefaultModel,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":    cfg.HaikuModel,
		"ANTHROPIC_DEFAULT_SONNET_MODEL":   cfg.SonnetModel,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":     cfg.OpusModel,
		"ANTHROPIC_DEFAULT_FABLE_MODEL":    cfg.FableModel,
		"ANTHROPIC_CUSTOM_MODEL_OPTION":    "",
		"CLAUDE_CODE_SUBAGENT_MODEL":       "inherit",
		"CLAUDE_CODE_SUBAGENT_MODEL_FORCE": "0",

		"DISABLE_TELEMETRY":                        "1",
		"DISABLE_ERROR_REPORTING":                  "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY":      "1",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":         "1",
		"CLAUDE_CODE_ATTRIBUTION_HEADER":           "0",
		"CLAUDE_CODE_HIDE_CWD":                     "1",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST":     "1",

		"DISABLE_AUTOUPDATER":                "1",
		"DISABLE_FEEDBACK_COMMAND":           "1",
		"DISABLE_BUG_COMMAND":                "1",
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

		"IS_DEMO": "1",

		"CLAUDE_CODE_IDE_SKIP_AUTO_INSTALL": "1",

		"ENABLE_CLAUDEAI_MCP_SERVERS": "false",

		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1",
	}

	if cfg.HaikuModel == "" {
		// Background calls must also use a model the gateway serves.
		vars["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = cfg.DefaultModel
	}

	return vars
}

func BuildArgs(cfg *ClaudeConfig) []string {
	args := []string{"--settings", `{"disableRemoteControl":true}`}
	if cfg != nil {
		// The SDK's parent policy channel is required: --settings alone does
		// not enforce managed model restrictions. Never change machine policy.
		policy, _ := json.Marshal(struct {
			AvailableModels      []string `json:"availableModels"`
			AvailableModelsMatch string   `json:"availableModelsMatch"`
			EnforceModels        bool     `json:"enforceAvailableModels"`
		}{cfg.Models, "exact", true})
		args = append(args, "--managed-settings", string(policy))
	}
	return args
}

func BuildEnv(parent []string, cfg *ClaudeConfig) []string {
	// Background tasks use ANTHROPIC_DEFAULT_HAIKU_MODEL instead of the
	// deprecated small/fast model override.
	return external.MergeEnv(parent, BuildVars(cfg), "ANTHROPIC_SMALL_FAST_MODEL")
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
