package junie

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

const profileName = "wingman"

func Run(ctx context.Context, args []string, options *Options) error {
	if options == nil {
		options = new(Options)
	}

	if options.Path == "" {
		options.Path = external.LookupPath("junie", "junie")
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

	seedState()

	cliArgs := append([]string{
		"--model-location", dir,
		"--model", "custom:" + profileName,
	}, args...)

	cmd := exec.CommandContext(ctx, options.Path, cliArgs...)
	cmd.Env = options.Env

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

// seedState writes ~/.junie state files to skip first-run prompts.
// Only adds missing fields — existing user values are never overwritten.
func seedState() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	junieDir := filepath.Join(home, ".junie")

	mergeJSON(filepath.Join(junieDir, "settings.json"), func(s map[string]any) bool {
		if _, ok := s["shareAnonymousStatistics"]; ok {
			return false
		}
		s["shareAnonymousStatistics"] = "false"
		return true
	})

	mergeJSON(filepath.Join(junieDir, "config.json"), func(s map[string]any) bool {
		if _, ok := s["auto-update"]; ok {
			return false
		}
		s["auto-update"] = false
		return true
	})

	cwd, err := os.Getwd()
	if err != nil {
		return
	}

	mergeJSON(filepath.Join(junieDir, "misc", "migration_state.json"), func(s map[string]any) bool {
		projects, _ := s["migratedProjects"].([]any)
		for _, p := range projects {
			if str, ok := p.(string); ok && str == cwd {
				return false
			}
		}
		s["migratedProjects"] = append(projects, cwd)
		return true
	})
}

// mergeJSON loads a JSON object, lets fn mutate it, and writes it back
// only if fn returns true. Best-effort: errors are swallowed.
func mergeJSON(path string, fn func(map[string]any) bool) {
	state := map[string]any{}

	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &state)
	}

	if !fn(state) {
		return
	}

	data, err := json.Marshal(state)
	if err != nil {
		return
	}

	_ = os.MkdirAll(filepath.Dir(path), 0755)
	_ = os.WriteFile(path, data, 0644)
}
