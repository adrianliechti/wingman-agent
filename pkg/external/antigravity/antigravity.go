package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/external"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
)

const appDataDir = "antigravity-cli"

func BinPath() string {
	return external.LookupPath("antigravity", "agy")
}

// ConfigDir returns a wingman-owned replacement for ~/.gemini. Gateway settings
// would otherwise switch the user's own Antigravity login to API-key mode.
func ConfigDir() (string, error) {
	dir, err := layout.WingmanPath("antigravity")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func BuildArgs(dir string, cfg *AntigravityConfig, args []string) []string {
	out := []string{"--gemini_dir", dir, "--app_data_dir", appDataDir}

	// Subcommands are positional. The settings file cannot select a custom model:
	// Antigravity 1.4 ignores IDs there and fails on labels.
	if cfg.Model != "" && !hasModelFlag(args) && (len(args) == 0 || strings.HasPrefix(args[0], "-")) {
		out = append(out, "--model", cfg.Model)
	}

	return append(out, args...)
}

func hasModelFlag(args []string) bool {
	for _, arg := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if strings.HasPrefix(arg, "-") && name == "model" {
			return true
		}
	}
	return false
}

func BuildEnv(parent []string, cfg *AntigravityConfig) []string {
	vars := map[string]string{
		"GOOGLE_GEMINI_BASE_URL": cfg.BaseURL,
		"GEMINI_API_KEY":         cfg.AuthToken,

		"AGY_CLI_DISABLE_AUTO_UPDATE": "1",
	}

	return external.MergeEnv(parent, vars, "AGY_ADC_AUTH")
}

// WriteSettings sets the gateway keys and keeps state Antigravity stores itself,
// such as trusted workspaces and the theme.
func WriteSettings(dir string, cfg *AntigravityConfig) error {
	path := filepath.Join(dir, appDataDir, "settings.json")

	settings := map[string]any{}

	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	models := make(map[string]any, len(cfg.Models))

	for _, m := range cfg.Models {
		models[m.ID] = map[string]any{
			"modelId":     m.ID,
			"modelName":   "gemini-api://" + m.ID,
			"label":       m.Name,
			"apiProvider": "API_PROVIDER_GOOGLE_GEMINI",
			"maxTokens":   m.ContextTokens(),

			"modelFeatures": map[string]any{
				"supportsToolCalls": true,
				"supportsImages":    external.IsAnthropic(m.ID) || external.IsOpenAI(m.ID) || external.IsGoogle(m.ID),
			},
		}
	}

	settings["modelProvider"] = "gemini"
	settings["enableTelemetry"] = false
	settings["customModelsConfig"] = map[string]any{"customModels": models}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "settings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
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

	dir, err := ConfigDir()
	if err != nil {
		return err
	}

	if err := WriteSettings(dir, cfg); err != nil {
		return err
	}

	return external.Run(ctx, options.Path, BuildArgs(dir, cfg, args), BuildEnv(options.Env, cfg))
}
