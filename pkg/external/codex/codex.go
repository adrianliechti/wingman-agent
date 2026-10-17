package codex

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

func BinPath() string {
	return external.LookupPath("codex", "codex")
}

func BuildArgs(cfg *CodexConfig) []string {
	url := strings.TrimRight(cfg.BaseURL, "/") + "/v1"

	args := []string{
		"--config", "agents.enabled=true",
		"--config", "features.multi_agent_v2=false",

		"--config", "model_provider=\"wingman\"",
		"--config", "model_providers.wingman.name=\"Wingman\"",
		"--config", "model_providers.wingman.base_url=" + tomlString(url),
		"--config", "model_providers.wingman.env_key=\"WINGMAN_TOKEN\"",
		"--config", "model_providers.wingman.wire_api=\"responses\"",
		"--config", "model_providers.wingman.requires_openai_auth=false",
		"--config", "model_providers.wingman.supports_websockets=false",
		"--config", "model_providers.wingman.supports_standalone_web_search=false",
		"--config", "model_providers.wingman.capabilities.external_web_access=false",
		// Keep the user's ChatGPT login separate from this gateway process.
		"--config", "cli_auth_credentials_store=\"ephemeral\"",

		"--config", "feedback.enabled=false",
		"--config", "analytics.enabled=false",
		"--config", "history.persistence=\"none\"",
		"--config", "otel.exporter=\"none\"",
		"--config", "otel.trace_exporter=\"none\"",
		"--config", "otel.metrics_exporter=\"none\"",
		"--config", "otel.log_user_prompt=false",

		"--config", "web_search=\"disabled\"",
		"--config", "features.apps=false",
		// Local plugins and MCP servers still load from the user's config.
		"--config", "features.remote_plugin=false",
		"--config", "features.plugin_sharing=false",
		"--config", "features.recommended_plugins=false",
		"--config", "features.tool_suggest=false",
		"--config", "features.fast_mode=false",
		"--config", "features.ultrafast_mode=false",
		"--config", "features.daemon_auto_start=false",
		"--config", "features.realtime_conversation=false",
		// Avoid persisting a snapshot of the parent shell's credentials.
		"--config", "features.shell_snapshot=false",
		"--config", "shell_environment_policy.ignore_default_excludes=false",
		"--config", "shell_environment_policy.set.WINGMAN_TOKEN=\"\"",

		"--config", "tui.show_tooltips=false",
		"--config", "check_for_update_on_startup=false",
	}

	if cfg.ModelCatalog != "" {
		args = append(args, "--config", "model_catalog_json="+tomlString(cfg.ModelCatalog))
		args = append(args, "--config", "features.api_key_model_discovery=false")
	}

	return args
}

func tomlString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func BuildEnv(parent []string, cfg *CodexConfig) []string {
	return external.MergeEnv(parent, map[string]string{"WINGMAN_TOKEN": cfg.AuthToken}, "OPENAI_API_KEY", "CODEX_API_KEY")
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

	cleanup, err := PrepareModelCatalog(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	return external.Run(ctx, options.Path, append(BuildArgs(cfg), args...), BuildEnv(options.Env, cfg))
}
