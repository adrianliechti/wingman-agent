package copilot

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func Run(ctx context.Context, args []string, options *Options) error {
	if options == nil {
		options = new(Options)
	}

	if options.Path == "" {
		options.Path = "copilot"
	}

	if options.Env == nil {
		options.Env = os.Environ()
	}

	cfg, err := NewConfig(ctx, options)

	if err != nil {
		return err
	}

	url := strings.TrimRight(cfg.BaseURL, "/") + "/v1/"

	vars := map[string]string{
		// Auth & API routing
		"COPILOT_PROVIDER_BASE_URL": url,
		"COPILOT_PROVIDER_API_KEY":  cfg.AuthToken,

		// Model configuration
		"COPILOT_MODEL":                      cfg.Model,
		"COPILOT_PROVIDER_MAX_PROMPT_TOKENS": strconv.Itoa(cfg.MaxPromptTokens),
		"COPILOT_PROVIDER_MAX_OUTPUT_TOKENS": strconv.Itoa(cfg.MaxOutputTokens),

		// Telemetry & data exfiltration prevention
		"COPILOT_OFFLINE": "true",
	}

	env := options.Env

	for k, v := range vars {
		env = append(env, k+"="+v)
	}

	cmd := exec.Command(options.Path, args...)
	cmd.Env = env

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
