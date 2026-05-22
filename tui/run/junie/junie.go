package junie

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const profileName = "wingman"

func Run(ctx context.Context, args []string, options *Options) error {
	if options == nil {
		options = new(Options)
	}

	if options.Path == "" {
		options.Path = "junie"
	}

	if options.Env == nil {
		options.Env = os.Environ()
	}

	cfg, err := NewConfig(ctx, options)

	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "wingman-junie-models-")

	if err != nil {
		return err
	}

	defer os.RemoveAll(dir)

	if err := writeProfile(dir, cfg); err != nil {
		return err
	}

	vars := map[string]string{
		// Model profile discovery & selection
		"JUNIE_MODEL_LOCATIONS": dir,
		"JUNIE_MODEL":           "custom:" + profileName,
	}

	env := stripVars(options.Env, vars)

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

func writeProfile(dir string, cfg *JunieConfig) error {
	profile := map[string]any{
		"id":      cfg.Model,
		"baseUrl": strings.TrimRight(cfg.BaseURL, "/") + "/v1/responses",
		"apiType": "OpenAIResponses",
		"apiKey":  cfg.AuthToken,
	}

	if cfg.FastModel != "" {
		profile["fasterModel"] = map[string]any{
			"id": cfg.FastModel,
		}
	}

	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, profileName+".json"), data, 0644)
}

func stripVars(env []string, vars map[string]string) []string {
	out := env[:0:0]

	for _, e := range env {
		key := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			key = e[:i]
		}
		if _, ok := vars[key]; ok {
			continue
		}
		out = append(out, e)
	}

	return out
}
