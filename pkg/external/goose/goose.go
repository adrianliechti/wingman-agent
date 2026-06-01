package goose

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

func Run(ctx context.Context, args []string, options *Options) error {
	if options == nil {
		options = new(Options)
	}

	if options.Path == "" {
		options.Path = external.LookupPath("goose", "goose")
	}

	if options.Env == nil {
		options.Env = os.Environ()
	}

	cfg, err := NewConfig(ctx, options)

	if err != nil {
		return err
	}

	host := strings.TrimRight(cfg.BaseURL, "/")

	vars := map[string]string{
		// Auth & API routing (OpenAI-compatible provider against Wingman)
		"GOOSE_PROVIDER":          "openai",
		"GOOSE_PROVIDER__TYPE":    "openai",
		"GOOSE_PROVIDER__HOST":    host,
		"GOOSE_PROVIDER__API_KEY": cfg.AuthToken,
		"OPENAI_HOST":             host,
		"OPENAI_BASE_PATH":        "v1/chat/completions",
		"OPENAI_API_KEY":          cfg.AuthToken,

		// Model configuration
		"GOOSE_MODEL":      cfg.Model,
		"GOOSE_FAST_MODEL": cfg.FastModel,

		// Telemetry & data exfiltration prevention
		// GOOSE_TELEMETRY_ENABLED is persisted to config.yaml below — see
		// ensureTelemetryDisabled — which also suppresses the first-run prompt.
		"OTEL_SDK_DISABLED":     "true",
		"OTEL_TRACES_EXPORTER":  "none",
		"OTEL_METRICS_EXPORTER": "none",
		"OTEL_LOGS_EXPORTER":    "none",

		// Disabled features (security & cost control)
		"GOOSE_DISABLE_KEYRING":           "1",
		"GOOSE_DISABLE_SESSION_NAMING":    "true",
		"GOOSE_DISABLE_TOOL_CALL_SUMMARY": "true",
		"GOOSE_RANDOM_THINKING_MESSAGES":  "false",
	}

	if cfg.ContextLimit > 0 {
		vars["GOOSE_CONTEXT_LIMIT"] = strconv.Itoa(cfg.ContextLimit)
	}

	// Persist the telemetry opt-out into the user's config.yaml so the
	// first-run consent dialog ("Share anonymous usage data...") is not
	// shown. The env var alone is not enough — Goose only suppresses the
	// prompt once the key is present in the file.
	_ = ensureTelemetryDisabled()

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

func configPath() (string, error) {
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", os.ErrNotExist
		}
		return filepath.Join(appData, "Block", "goose", "config", "config.yaml"), nil
	}

	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "goose", "config.yaml"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".config", "goose", "config.yaml"), nil
}

func ensureTelemetryDisabled() error {
	path, err := configPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "GOOSE_TELEMETRY_ENABLED") {
			return nil
		}
	}

	content := string(data)
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "GOOSE_TELEMETRY_ENABLED: false\n"

	return os.WriteFile(path, []byte(content), 0644)
}
