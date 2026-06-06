package gemini

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

func Run(ctx context.Context, args []string, options *Options) error {
	if options == nil {
		options = new(Options)
	}

	if options.Path == "" {
		options.Path = external.LookupPath("gemini", "gemini")
	}

	if options.Env == nil {
		options.Env = os.Environ()
	}

	cfg, err := NewConfig(ctx, options)

	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "gemini-config-*")

	if err != nil {
		return err
	}

	defer os.RemoveAll(dir)

	settings := map[string]any{

		"telemetry": map[string]any{
			"enabled":    false,
			"logPrompts": false,
		},
		"privacy": map[string]any{
			"usageStatisticsEnabled": false,
		},

		"general": map[string]any{
			"enableAutoUpdate":             false,
			"enableAutoUpdateNotification": false,
		},

		"experimental": map[string]any{
			"extensionRegistry": false,
		},
		"admin": map[string]any{
			"extensions": map[string]any{
				"enabled": false,
			},
		},
	}

	data, err := json.Marshal(settings)

	if err != nil {
		return err
	}

	settingsFile := filepath.Join(dir, "settings.json")

	if err := os.WriteFile(settingsFile, data, 0644); err != nil {
		return err
	}

	vars := map[string]string{

		"GOOGLE_GEMINI_BASE_URL":        cfg.BaseURL,
		"GEMINI_DEFAULT_AUTH_TYPE":      "gemini-api-key",
		"GEMINI_API_KEY":                cfg.AuthToken,
		"GEMINI_API_KEY_AUTH_MECHANISM": "bearer",

		"GEMINI_MODEL": cfg.Model,

		"GEMINI_CLI_SYSTEM_SETTINGS_PATH": settingsFile,
	}

	env := options.Env

	for k, v := range vars {
		env = append(env, k+"="+v)
	}

	cmd := exec.CommandContext(ctx, options.Path, args...)
	cmd.Env = env

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
