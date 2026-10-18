package agent

import (
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

// Options controls the built-in tools exposed by an Agent. Environment
// variables are resolved once when the agent starts and are combined with the
// explicitly supplied options.
type Options struct {
	// RequireSessionContext disables single-client prompt routing fallbacks.
	RequireSessionContext  bool
	IsolateSessionSettings bool
	DisableShell           bool
	DisableSandbox         bool
	DisableWebSearch       bool
	DisableWebFetch        bool

	Sandbox *SandboxOptions

	// Telemetry overrides the pipeline on the underlying agent.Config. A nil
	// value inherits that config, including automatic standard OTEL environment
	// configuration from agent.DefaultConfig.
	Telemetry *telemetry.Telemetry

	// ShutdownTelemetryOnClose transfers ownership of the selected telemetry
	// pipeline to this Agent. The Agent closes associated workspace MCP sessions
	// before shutting the pipeline down so their final metrics can be exported.
	// Most embedders should leave ownership with the component that created the
	// pipeline.
	ShutdownTelemetryOnClose bool
}

// SandboxOptions configures the OS sandbox used for shell commands and
// scheduled scripts. A nil value or nil fields inherit the environment
// defaults (WINGMAN_SANDBOX_NETWORK).
type SandboxOptions struct {
	Network *bool
}

func resolveOptions(options []Options) Options {
	resolved := Options{
		DisableShell:     environmentEnabled("WINGMAN_DISABLE_SHELL"),
		DisableSandbox:   sandboxDisabledFromEnv(),
		DisableWebSearch: environmentEnabled("WINGMAN_DISABLE_WEBSEARCH"),
		DisableWebFetch:  environmentEnabled("WINGMAN_DISABLE_WEBFETCH"),
		Sandbox: &SandboxOptions{
			Network: new(environmentEnabled("WINGMAN_SANDBOX_NETWORK")),
		},
	}
	for _, option := range options {
		resolved.RequireSessionContext = resolved.RequireSessionContext || option.RequireSessionContext
		resolved.IsolateSessionSettings = resolved.IsolateSessionSettings || option.IsolateSessionSettings
		resolved.DisableShell = resolved.DisableShell || option.DisableShell
		resolved.DisableSandbox = resolved.DisableSandbox || option.DisableSandbox
		resolved.DisableWebSearch = resolved.DisableWebSearch || option.DisableWebSearch
		resolved.DisableWebFetch = resolved.DisableWebFetch || option.DisableWebFetch
		if option.Sandbox != nil && option.Sandbox.Network != nil {
			resolved.Sandbox.Network = new(*option.Sandbox.Network)
		}
		if option.Telemetry != nil {
			resolved.Telemetry = option.Telemetry
		}
		resolved.ShutdownTelemetryOnClose = resolved.ShutdownTelemetryOnClose || option.ShutdownTelemetryOnClose
	}
	return resolved
}

func sandboxDisabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WINGMAN_SANDBOX"))) {
	case "off", "false", "0", "no", "disabled":
		return true
	default:
		return false
	}
}

func environmentEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
