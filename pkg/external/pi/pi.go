package pi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

const providerName = "wingman"

func BinPath() string {
	return external.LookupPath("pi", "pi")
}

func Run(ctx context.Context, args []string, options *Options) error {
	if options == nil {
		options = new(Options)
	}

	if options.Path == "" {
		options.Path = BinPath()
	}

	if options.Env == nil {
		options.Env = os.Environ()
	}

	cfg, err := NewConfig(ctx, options)

	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "pi-config-*")

	if err != nil {
		return err
	}

	defer os.RemoveAll(dir)

	if err := writeModels(dir, cfg); err != nil {
		return err
	}

	vars := map[string]string{
		"PI_CODING_AGENT_DIR": dir,

		"PI_OFFLINE":            "1",
		"PI_SKIP_VERSION_CHECK": "1",
	}

	env := options.Env

	for k, v := range vars {
		env = append(env, k+"="+v)
	}

	args = append(buildArgs(cfg), args...)

	cmd := exec.CommandContext(ctx, options.Path, args...)
	cmd.Env = env

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func buildArgs(cfg *PiConfig) []string {
	args := []string{"--provider", providerName}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}

	return args
}

func writeModels(dir string, cfg *PiConfig) error {
	type model struct {
		ID string `json:"id"`
	}

	type provider struct {
		BaseURL string  `json:"baseUrl"`
		API     string  `json:"api"`
		APIKey  string  `json:"apiKey"`
		Models  []model `json:"models"`
	}

	models := make([]model, 0, len(cfg.Models))

	for _, id := range cfg.Models {
		models = append(models, model{ID: id})
	}

	doc := map[string]any{
		"providers": map[string]any{
			providerName: provider{
				BaseURL: strings.TrimRight(cfg.BaseURL, "/") + "/v1",
				API:     "openai-completions",
				APIKey:  cfg.AuthToken,
				Models:  models,
			},
		},
	}

	data, err := json.MarshalIndent(doc, "", "  ")

	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "models.json"), data, 0600)
}
