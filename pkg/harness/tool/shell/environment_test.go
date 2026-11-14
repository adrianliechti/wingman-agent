package shell

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestDefaultEnvironmentPolicyScrubsAgentCredentials(t *testing.T) {
	policy := DefaultEnvironmentPolicy()
	got := policy.Filter([]string{
		"PATH=/bin",
		"OPENAI_API_KEY=openai-secret",
		"ANTHROPIC_AUTH_TOKEN=anthropic-secret",
		"ANTHROPIC_BASE_URL=https://example.test",
		"GITHUB_TOKEN=workflow-token",
	})
	joined := strings.Join(got, "\n")
	for _, secret := range []string{"openai-secret", "anthropic-secret", "ANTHROPIC_BASE_URL", "workflow-token"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("filtered environment leaked %q: %s", secret, joined)
		}
	}
	if !strings.Contains(joined, "PATH=/bin") {
		t.Fatalf("ordinary development environment was removed: %s", joined)
	}
}

func TestEnvironmentPolicyAllowAndReplaceOverrideDeny(t *testing.T) {
	policy := EnvironmentPolicy{
		Deny:    []string{"SECRET_*"},
		Allow:   []string{"SECRET_NEEDED"},
		Replace: map[string]string{"SECRET_REPLACED": "session-value"},
	}
	got := strings.Join(policy.Filter([]string{
		"SECRET_DROP=host-value",
		"SECRET_NEEDED=allowed-value",
		"SECRET_REPLACED=host-value",
	}), "\n")
	if strings.Contains(got, "SECRET_DROP") || strings.Contains(got, "host-value") {
		t.Fatalf("denied values survived: %s", got)
	}
	if !strings.Contains(got, "SECRET_NEEDED=allowed-value") || !strings.Contains(got, "SECRET_REPLACED=session-value") {
		t.Fatalf("explicit overrides missing: %s", got)
	}
}

func TestToolCommandUsesFilteredEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	t.Setenv("WINGMAN_TEST_VISIBLE", "visible")

	workDir := t.TempDir()
	cmd, err := buildToolCommand(context.Background(), environmentPrintCommand(), workDir, workDir, &Options{DisableSandbox: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "must-not-leak") || !strings.Contains(text, "visible") {
		t.Fatalf("unexpected tool environment: %q", text)
	}
}

func TestCustomEnvironmentPolicyExtendsDefault(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "default-denied")
	t.Setenv("WINGMAN_TEST_SECRET", "custom-denied")
	t.Setenv("WINGMAN_TEST_VISIBLE", "visible")

	opts := &Options{Environment: &EnvironmentPolicy{Deny: []string{"WINGMAN_TEST_SECRET"}}}
	environ := strings.Join(environmentForTools(opts, false), "\n")
	if strings.Contains(environ, "default-denied") || strings.Contains(environ, "custom-denied") {
		t.Fatalf("custom policy disabled a deny rule: %s", environ)
	}
	if !strings.Contains(environ, "WINGMAN_TEST_VISIBLE=visible") {
		t.Fatalf("custom policy removed an ordinary variable: %s", environ)
	}
	// Escalated commands keep the user's credentials but not the host's own denials.
	escalated := strings.Join(environmentForTools(opts, true), "\n")
	if !strings.Contains(escalated, "default-denied") || strings.Contains(escalated, "custom-denied") {
		t.Fatalf("escalated environment: %s", escalated)
	}
}

func TestToolCommandTempOutsideSandbox(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	workspace, sessionTemp := t.TempDir(), t.TempDir()
	for _, tc := range []struct {
		name                string
		disabled, escalated bool
	}{
		{name: "sandbox disabled", disabled: true},
		{name: "escalated", escalated: true},
	} {
		cmd, err := buildToolCommand(t.Context(), "exit 0", workspace, workspace, &Options{
			TempDir: sessionTemp, DisableSandbox: tc.disabled,
		}, tc.escalated)
		if err != nil {
			t.Fatal(err)
		}
		environ := "\n" + strings.Join(cmd.Env, "\n")
		for _, key := range []string{"TMPDIR", "TEMP", "TMP"} {
			if !strings.Contains(environ, "\n"+key+"="+sessionTemp) {
				t.Errorf("%s: %s does not use private temp", tc.name, key)
			}
		}
	}
}

func environmentPrintCommand() string {
	if os.PathSeparator == '\\' {
		return `[Console]::WriteLine("$env:OPENAI_API_KEY|$env:WINGMAN_TEST_VISIBLE")`
	}
	return `printf '%s|%s' "$OPENAI_API_KEY" "$WINGMAN_TEST_VISIBLE"`
}

func TestSandboxedCommandsReadTheGoBuildCacheWithoutWritingIt(t *testing.T) {
	for name, tc := range map[string]struct {
		environment []string
		want        string
	}{
		"unset":             {[]string{"PATH=/bin"}, "GODEBUG=goindex=0"},
		"empty":             {[]string{"GODEBUG="}, "GODEBUG=goindex=0"},
		"other settings":    {[]string{"GODEBUG=gctrace=1,"}, "GODEBUG=gctrace=1,goindex=0"},
		"user chose index":  {[]string{"GODEBUG=goindex=1"}, "GODEBUG=goindex=1"},
		"already read-only": {[]string{"GODEBUG=http2debug=1,goindex=0"}, "GODEBUG=http2debug=1,goindex=0"},
	} {
		got := environmentReadOnlyCaches(append([]string(nil), tc.environment...))
		var values []string
		for _, entry := range got {
			if strings.HasPrefix(entry, "GODEBUG=") {
				values = append(values, entry)
			}
		}
		if len(values) != 1 || values[0] != tc.want {
			t.Errorf("%s: GODEBUG entries = %q, want %q", name, values, tc.want)
		}
	}

	t.Setenv(workspaceSandboxActiveEnv, "")
	t.Setenv("GODEBUG", "")
	workspace := t.TempDir()
	godebug := func(opts *Options, escalated bool) string {
		t.Helper()
		cmd, err := buildToolCommand(t.Context(), "exit 0", workspace, workspace, opts, escalated)
		if err != nil {
			t.Skipf("sandbox unavailable: %v", err)
		}
		for _, entry := range cmd.Env {
			if value, ok := strings.CutPrefix(entry, "GODEBUG="); ok {
				return value
			}
		}
		return ""
	}
	if got := godebug(&Options{}, false); got != "goindex=0" {
		t.Errorf("sandboxed command GODEBUG = %q, want goindex=0", got)
	}
	// Outside the sandbox the cache is writable and the index stays on.
	if got := godebug(&Options{DisableSandbox: true}, false); got != "" {
		t.Errorf("unsandboxed command GODEBUG = %q, want it untouched", got)
	}
	if got := godebug(&Options{}, true); got != "" {
		t.Errorf("escalated command GODEBUG = %q, want it untouched", got)
	}
}
