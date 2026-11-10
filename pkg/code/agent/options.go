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

	DisableShell   bool
	DisableSandbox bool
	SandboxNetwork bool
	// SandboxTemp lets sandboxed commands write /tmp and the per-user temporary
	// directory as well as their private one; WINGMAN_SANDBOX_TEMP=off turns it off.
	SandboxTemp bool
	// DisableSandboxLoopback is set by WINGMAN_SANDBOX_LOOPBACK=off.
	DisableSandboxLoopback bool
	// SandboxCredentials lets commands read credential stores and keep
	// authentication variables; WINGMAN_SANDBOX_CREDENTIALS=off withholds both.
	SandboxCredentials bool
	DisableWebSearch   bool
	DisableWebFetch    bool

	// Empty role models are selected automatically from available models.
	ComplexModel string
	UtilityModel string
}

// optionsFromEnvironment reads startup settings for DefaultConfig.
func optionsFromEnvironment() Options {
	return Options{
		DisableShell:           environmentEnabled("WINGMAN_DISABLE_SHELL"),
		DisableSandbox:         sandboxDisabledFromEnv(),
		SandboxNetwork:         environmentEnabled("WINGMAN_SANDBOX_NETWORK"),
		SandboxTemp:            environmentSwitch("WINGMAN_SANDBOX_TEMP") != "off",
		DisableSandboxLoopback: environmentSwitch("WINGMAN_SANDBOX_LOOPBACK") == "off",
		SandboxCredentials:     environmentSwitch("WINGMAN_SANDBOX_CREDENTIALS") != "off",
		DisableWebSearch:       environmentEnabled("WINGMAN_DISABLE_WEBSEARCH"),
		DisableWebFetch:        environmentEnabled("WINGMAN_DISABLE_WEBFETCH"),
		ComplexModel:           harness.DefaultComplexModel(),
		UtilityModel:           harness.DefaultUtilityModel(),
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

// environmentSwitch reads a setting that can be forced on or off; empty leaves the default.
func environmentSwitch(name string) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "on", "true", "1", "yes", "enabled":
		return "on"
	case "off", "false", "0", "no", "disabled":
		return "off"
	}
	return ""
}

func environmentEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
