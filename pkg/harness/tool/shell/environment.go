package shell

import (
	"os"
	"runtime"
	"slices"
	"strings"
)

// Options controls the host capabilities exposed to model-generated commands.
type Options struct {
	// ScratchDir receives complete shell transcripts when inline capture is
	// exceeded. Empty disables transcript persistence while retaining head/tail.
	ScratchDir string
	// TempDir is private to the session. Shared host temp directories are never
	// granted to commands merely because they are temporary storage.
	TempDir string
	// DisableSandbox runs model-generated commands without the OS sandbox.
	DisableSandbox bool
	// Network grants IP networking for this execution while retaining the
	// filesystem sandbox. Only the approval path may set this per command.
	Network bool
	// DisableLoopback keeps commands without Network off the host's loopback
	// interface on macOS, where it also reaches other local services.
	DisableLoopback bool
	// ReadCredentials lets commands use the user's credentials, as Codex does:
	// credential stores stay readable inside the sandbox and authentication
	// variables stay in the environment. Without it both are withheld.
	ReadCredentials bool
	// SharedTemp also lets sandboxed commands write the host's shared temporary
	// directories, for tools that ignore TMPDIR such as mktemp on macOS.
	SharedTemp bool

	// ExtraWritableRoots grants the workspace sandbox write access to
	// directories outside the workspace, such as project memory and scratch data.
	ExtraWritableRoots []string

	// Environment extends DefaultEnvironmentPolicy. Allow can explicitly keep
	// a default-denied key; Deny adds patterns; Replace sets final values.
	// The WINGMAN_ENV_ALLOW variable ("KEY1,PREFIX_*") also extends Allow, so
	// denied keys can be restored without embedding code changes.
	Environment *EnvironmentPolicy
}

// EnvironmentPolicy filters the environment inherited by model-generated
// commands. Allow takes precedence over Deny. Replace is applied last and can
// intentionally reintroduce an otherwise denied variable.
type EnvironmentPolicy struct {
	Allow   []string
	Deny    []string
	Replace map[string]string
}

var defaultDeniedEnvironment = []string{
	"ANTHROPIC_*",
	"OPENAI_API_KEY",
	"OPENAI_ADMIN_KEY",
	"AZURE_OPENAI_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"WINGMAN_API_KEY",
	"WINGMAN_AUTH_TOKEN",
	"WINGMAN_TOKEN",
	"GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN", "NPM_TOKEN", "NODE_AUTH_TOKEN",
	"AWS_*", "AZURE_*", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_AUTH_*",
	"SSH_AUTH_SOCK", "SSH_AGENT_PID", "DOCKER_HOST", "DOCKER_CONTEXT",
	"DBUS_SESSION_BUS_ADDRESS", "KUBECONFIG",
}

func DefaultEnvironmentPolicy() EnvironmentPolicy {
	return EnvironmentPolicy{Deny: append([]string(nil), defaultDeniedEnvironment...)}
}

// environmentForTools filters the host environment. Escalated commands run with
// the user's privileges, so they keep the credentials the default policy removes.
func environmentForTools(opts *Options, escalated bool) []string {
	policy := DefaultEnvironmentPolicy()
	if escalated || (opts != nil && opts.ReadCredentials) {
		policy.Deny = nil
	}
	for pattern := range strings.SplitSeq(os.Getenv("WINGMAN_ENV_ALLOW"), ",") {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			policy.Allow = append(policy.Allow, pattern)
		}
	}
	if opts != nil && opts.Environment != nil {
		policy.Allow = append(policy.Allow, opts.Environment.Allow...)
		policy.Deny = append(policy.Deny, opts.Environment.Deny...)
		policy.Replace = opts.Environment.Replace
	}
	return policy.Filter(os.Environ())
}

// Only temp is redirected: a cache variable would override the tool's configured path (go env, .npmrc).
func environmentWithTemp(environment []string, tempDir string) []string {
	if tempDir != "" {
		for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
			environment = setEnvironment(environment, key, tempDir)
		}
	}
	return environment
}

// offlineDefaults stop package managers from retrying when the sandbox has no
// outside network. npm otherwise takes over a minute to report the failure
// that tells the harness, and the model, that the command needs network access.
// pip keeps one retry: without any it no longer prints the connection error.
var offlineDefaults = [][2]string{
	{"npm_config_fetch_retries", "0"},
	{"PIP_RETRIES", "1"},
	{"CARGO_NET_RETRY", "0"},
}

// environmentOffline adds offlineDefaults, keeping values the user configured.
func environmentOffline(environment []string) []string {
	for _, entry := range offlineDefaults {
		key := entry[0]
		if !slices.ContainsFunc(environment, func(existing string) bool {
			name, _, _ := strings.Cut(existing, "=")
			return strings.EqualFold(name, key)
		}) {
			environment = append(environment, key+"="+entry[1])
		}
	}
	return environment
}

// Filter applies the policy to KEY=value entries. Matching is
// case-insensitive on Windows and supports a trailing * prefix pattern.
func (p EnvironmentPolicy) Filter(environ []string) []string {
	out := make([]string, 0, len(environ)+len(p.Replace))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if matchesEnvironmentKey(key, p.Deny) && !matchesEnvironmentKey(key, p.Allow) {
			continue
		}
		out = append(out, entry)
	}
	for key, value := range p.Replace {
		out = setEnvironment(out, key, value)
	}
	return out
}

func matchesEnvironmentKey(key string, patterns []string) bool {
	if runtime.GOOS == "windows" {
		key = strings.ToUpper(key)
	}
	for _, pattern := range patterns {
		if runtime.GOOS == "windows" {
			pattern = strings.ToUpper(pattern)
		}
		if prefix, wildcard := strings.CutSuffix(pattern, "*"); wildcard {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		} else if key == pattern {
			return true
		}
	}
	return false
}

func setEnvironment(environ []string, key, value string) []string {
	filtered := environ[:0]
	for _, entry := range environ {
		entryKey, _, ok := strings.Cut(entry, "=")
		if ok && environmentKeysEqual(entryKey, key) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, key+"="+value)
}

func environmentKeysEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
