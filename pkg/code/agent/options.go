package agent

import (
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

// Options toggles code agent features; the zero value enables all tools with sandboxing.
type Options struct {
	// IsolateSessions disables single-client prompt routing fallbacks and keeps model
	// and effort changes scoped to the session where they were made.
	IsolateSessions bool

	DisableShell     bool
	DisableSandbox   bool
	SandboxNetwork   bool
	DisableWebSearch bool
	DisableWebFetch  bool

	// Empty role models are selected automatically from available models.
	ComplexModel string
	UtilityModel string
}

// optionsFromEnvironment reads startup settings for DefaultConfig.
func optionsFromEnvironment() Options {
	return Options{
		DisableShell:     environmentEnabled("WINGMAN_DISABLE_SHELL"),
		DisableSandbox:   sandboxDisabledFromEnv(),
		SandboxNetwork:   environmentEnabled("WINGMAN_SANDBOX_NETWORK"),
		DisableWebSearch: environmentEnabled("WINGMAN_DISABLE_WEBSEARCH"),
		DisableWebFetch:  environmentEnabled("WINGMAN_DISABLE_WEBFETCH"),
		ComplexModel:     harness.DefaultComplexModel(),
		UtilityModel:     harness.DefaultUtilityModel(),
	}
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
