package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// BuildVars returns the env-var name→value pairs that route the Claude
// Code CLI to the configured Wingman server and lock down telemetry /
// non-essential traffic. The host merges them onto whatever parent env is
// being passed to the subprocess (see [BuildEnv]).
func BuildVars(cfg *ClaudeConfig) map[string]string {
	vars := map[string]string{
		// Auth & API routing
		"ANTHROPIC_BASE_URL":   cfg.BaseURL,
		"ANTHROPIC_API_KEY":    "",
		"ANTHROPIC_AUTH_TOKEN": cfg.AuthToken,

		// Telemetry & data exfiltration prevention
		"DISABLE_TELEMETRY":                        "1",
		"DISABLE_ERROR_REPORTING":                  "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY":      "1",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":         "1",
		"CLAUDE_CODE_MCP_ALLOWLIST_ENV":            "1",
		"CLAUDE_CODE_SKIP_PROMPT_HISTORY":          "1",
		"CLAUDE_CODE_ATTRIBUTION_HEADER":           "0",
		"CLAUDE_CODE_HIDE_CWD":                     "1",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST":     "1",

		// Disabled commands (not applicable in managed environment)
		"DISABLE_AUTOUPDATER":                "1",
		"DISABLE_FEEDBACK_COMMAND":           "1",
		"DISABLE_INSTALLATION_CHECKS":        "1",
		"DISABLE_EXTRA_USAGE_COMMAND":        "1",
		"DISABLE_UPGRADE_COMMAND":            "1",
		"DISABLE_DOCTOR_COMMAND":             "1",
		"DISABLE_INSTALL_GITHUB_APP_COMMAND": "1",
		"DISABLE_LOGIN_COMMAND":              "1",
		"DISABLE_LOGOUT_COMMAND":             "1",

		// Disabled features (security & cost control)
		"CLAUDE_CODE_DISABLE_FAST_MODE":             "1",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS":      "1",
		"CLAUDE_CODE_DISABLE_CRON":                  "1",
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS":    "1",
		"CLAUDE_CODE_DISABLE_1M_CONTEXT":            "1",
		"CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK": "1",
		"CLAUDE_CODE_DISABLE_LEGACY_MODEL_REMAP":    "1",

		// UI & integration lockdown
		"CLAUDE_CODE_HIDE_ACCOUNT_INFO":     "1",
		"CLAUDE_CODE_IDE_SKIP_AUTO_INSTALL": "1",

		"ENABLE_CLAUDEAI_MCP_SERVERS": "false",

		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1",
	}

	if cfg.HaikuModel != "" {
		vars["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = cfg.HaikuModel
	}

	if cfg.SonnetModel != "" {
		vars["ANTHROPIC_DEFAULT_SONNET_MODEL"] = cfg.SonnetModel
	}

	if cfg.OpusModel != "" {
		vars["ANTHROPIC_DEFAULT_OPUS_MODEL"] = cfg.OpusModel
	}

	if cfg.SonnetModel != "" {
		vars["CLAUDE_CODE_SUBAGENT_MODEL"] = cfg.SonnetModel
	}

	return vars
}

// BuildEnv returns parent appended with [BuildVars]. If parent is nil,
// os.Environ() is used as the baseline. The result is a KEY=value slice
// suitable for assignment to exec.Cmd.Env.
func BuildEnv(parent []string, cfg *ClaudeConfig) []string {
	if parent == nil {
		parent = os.Environ()
	}
	env := make([]string, 0, len(parent)+len(BuildVars(cfg)))
	env = append(env, parent...)
	for k, v := range BuildVars(cfg) {
		env = append(env, k+"="+v)
	}
	return env
}

// FindPath locates the `claude` binary, preferring $PATH and falling back
// to the native installer's default locations (~/.local/bin on all
// platforms, plus the legacy ~/.claude/local).
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
	if options == nil {
		options = new(Options)
	}

	if options.Path == "" {
		path, err := FindPath()
		if err != nil {
			return err
		}

		options.Path = path
	}

	if options.Env == nil {
		options.Env = os.Environ()
	}

	cfg, err := NewConfig(ctx, options)

	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, options.Path, args...)
	cmd.Env = BuildEnv(options.Env, cfg)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
