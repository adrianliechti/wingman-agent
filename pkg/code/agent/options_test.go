package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
)

func TestOptionsFromEnvironment(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("WINGMAN_DISABLE_SHELL", value)
			t.Setenv("WINGMAN_SANDBOX_NETWORK", value)
			resolved := optionsFromEnvironment()
			if !resolved.DisableShell || !resolved.SandboxNetwork {
				t.Fatalf("WINGMAN_DISABLE_SHELL/WINGMAN_SANDBOX_NETWORK=%q did not enable options: %+v", value, resolved)
			}
		})
	}

	for _, value := range []string{"", "0", "false", "no", "off", "anything"} {
		t.Run("ignored_"+value, func(t *testing.T) {
			t.Setenv("WINGMAN_DISABLE_SHELL", value)
			t.Setenv("WINGMAN_SANDBOX_NETWORK", value)
			resolved := optionsFromEnvironment()
			if resolved.DisableShell || resolved.SandboxNetwork {
				t.Fatalf("WINGMAN_DISABLE_SHELL/WINGMAN_SANDBOX_NETWORK=%q enabled options: %+v", value, resolved)
			}
		})
	}

	for _, value := range []string{"off", "false", "0", "no", "disabled", " OFF "} {
		t.Run("sandbox_off_"+value, func(t *testing.T) {
			t.Setenv("WINGMAN_SANDBOX", value)
			if !optionsFromEnvironment().DisableSandbox {
				t.Fatalf("WINGMAN_SANDBOX=%q did not disable sandbox", value)
			}
		})
	}

	for _, value := range []string{"", "workspace", "workspace-write", "WORKSPACE", "on", "true", "1", "typo"} {
		t.Run("sandbox_on_"+value, func(t *testing.T) {
			t.Setenv("WINGMAN_SANDBOX", value)
			if optionsFromEnvironment().DisableSandbox {
				t.Fatalf("WINGMAN_SANDBOX=%q disabled sandbox", value)
			}
		})
	}

	t.Setenv("WINGMAN_DISABLE_SHELL", "")
	t.Setenv("WINGMAN_SANDBOX", "off")
	t.Setenv("WINGMAN_SANDBOX_NETWORK", "on")
	t.Setenv("WINGMAN_DISABLE_WEBSEARCH", "yes")
	t.Setenv("WINGMAN_DISABLE_WEBFETCH", "on")
	resolved := optionsFromEnvironment()
	if !resolved.DisableWebSearch || !resolved.DisableWebFetch || !resolved.DisableSandbox || !resolved.SandboxNetwork {
		t.Fatalf("resolved options = %+v", resolved)
	}
}

func TestOptionsFromEnvironmentIgnoresSeparatedWebEnvironmentNames(t *testing.T) {
	t.Setenv("WINGMAN_DISABLE_WEBSEARCH", "")
	t.Setenv("WINGMAN_DISABLE_WEBFETCH", "")
	t.Setenv("WINGMAN_DISABLE_WEB_SEARCH", "1")
	t.Setenv("WINGMAN_DISABLE_WEB_FETCH", "1")

	resolved := optionsFromEnvironment()
	if resolved.DisableWebSearch || resolved.DisableWebFetch {
		t.Fatalf("separated names unexpectedly changed options: %+v", resolved)
	}
}

func TestExplicitOptionsDisableOnlySelectedToolFamilies(t *testing.T) {
	workspace := newOptionsTestWorkspace(t)

	for _, test := range []struct {
		name    string
		options Options
		absent  []string
		present []string
	}{
		{
			name:    "shell",
			options: Options{DisableShell: true},
			absent:  []string{"shell", "exec", "exec_session"},
			present: []string{"fetch", "search_web"},
		},
		{
			name:    "web search",
			options: Options{DisableWebSearch: true},
			absent:  []string{"search_web"},
			present: []string{"exec", "exec_session", "fetch"},
		},
		{
			name:    "web fetch",
			options: Options{DisableWebFetch: true},
			absent:  []string{"fetch"},
			present: []string{"exec", "exec_session", "search_web"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := New(workspace, Config{Options: test.options}, nil)
			defer a.Close()
			sessionID, err := a.NewSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			assertTools(t, a, sessionID, test.absent, test.present)
		})
	}
}

func TestEnvironmentOptionsAreResolvedAtAgentStartup(t *testing.T) {
	workspace := newOptionsTestWorkspace(t)
	t.Setenv("WINGMAN_DISABLE_SHELL", "1")
	t.Setenv("WINGMAN_DISABLE_WEBSEARCH", "true")
	t.Setenv("WINGMAN_DISABLE_WEBFETCH", "yes")

	options := optionsFromEnvironment()

	// The environment is a startup input, not mutable per-session state.
	t.Setenv("WINGMAN_DISABLE_SHELL", "")
	t.Setenv("WINGMAN_DISABLE_WEBSEARCH", "")
	t.Setenv("WINGMAN_DISABLE_WEBFETCH", "")

	a := New(workspace, Config{Options: options}, nil)
	defer a.Close()
	options.DisableShell = false
	options.DisableWebSearch = false
	options.DisableWebFetch = false

	sessionID, err := a.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertTools(t, a, sessionID,
		[]string{"shell", "exec", "exec_session", "search_web", "fetch", "todo"},
		[]string{"read", "edit"},
	)
}

func TestNewUsesExplicitSettingsWithoutEnvironmentMerge(t *testing.T) {
	workspace := newOptionsTestWorkspace(t)
	t.Setenv("WINGMAN_DISABLE_SHELL", "1")
	t.Setenv("WINGMAN_DISABLE_WEBSEARCH", "1")
	t.Setenv("WINGMAN_DISABLE_WEBFETCH", "1")
	t.Setenv("WINGMAN_SANDBOX_NETWORK", "1")
	t.Setenv("WINGMAN_MODEL", "environment-model")
	t.Setenv("WINGMAN_EFFORT", "low")
	t.Setenv("WINGMAN_MODEL_COMPLEX", "environment-complex")
	t.Setenv("WINGMAN_MODEL_UTILITY", "environment-utility")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("WINGMAN_OUTPUT_TOKEN_BUDGET", "")
	t.Setenv("WINGMAN_TASK_MAX_TOKENS", "")
	t.Setenv("WINGMAN_TASK_TIMEOUT", "")
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model() != "environment-model" || cfg.Effort() != "low" || cfg.ComplexModel != "environment-complex" || cfg.UtilityModel != "environment-utility" || !cfg.DisableShell || !cfg.DisableWebSearch || !cfg.DisableWebFetch || !cfg.SandboxNetwork {
		t.Fatal("DefaultConfig did not load model and tool settings together")
	}
	cfg.Model = func() string { return "gpt-6-sol" }
	cfg.Effort = func() string { return "high" }
	cfg.MaxTurns = 7
	cfg.DisableShell = false
	cfg.DisableWebSearch = false
	cfg.DisableWebFetch = false
	cfg.SandboxNetwork = false
	cfg.ComplexModel = "gpt-6-astra"
	cfg.UtilityModel = "gpt-6-luna"
	a := New(workspace, cfg, nil)
	defer a.Close()

	// Changing caller-owned startup values must not change this agent or future sessions.
	cfg.Model = func() string { return "changed-model" }
	cfg.Effort = func() string { return "low" }
	cfg.MaxTurns = 1
	cfg.DisableShell = true
	if a.modelID != "gpt-6-sol" || a.effort != "high" || a.complexModel != "gpt-6-astra" || a.utilityModel != "gpt-6-luna" {
		t.Fatalf("unexpected startup model settings: %q / %q / %q / %q", a.modelID, a.effort, a.complexModel, a.utilityModel)
	}
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assertTools(t, a, id, nil, []string{"exec", "exec_session", "search_web", "fetch"})
	s := a.session(id)
	if s.harness.MaxTurns != 7 || s.harness.Model() != "gpt-6-sol" || s.harness.Effort() != "high" {
		t.Fatal("session did not retain its startup config")
	}
	if s.scriptOptions.Shell.Network || s.scriptOptions.Shell.DisableSandbox {
		t.Fatal("session did not retain explicit sandbox settings")
	}
}

func TestConfigHooksAreOwnedByAgentAndSessions(t *testing.T) {
	workspace := newOptionsTestWorkspace(t)
	var inherited, replaced int
	cfg := Config{}
	cfg.Hooks.SessionEnd = []hook.SessionEnd{func(context.Context, string) { inherited++ }}
	a := New(workspace, cfg, nil)
	defer a.Close()

	// Caller changes after construction must not affect future sessions.
	cfg.Hooks.SessionEnd[0] = func(context.Context, string) { t.Error("caller hook reached a session") }
	first, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.session(first).harness.Hooks.SessionEnd) != 1 {
		t.Fatal("session lost the configured lifecycle hook")
	}
	// A session's changes must not affect its siblings or the startup config.
	a.session(first).harness.Hooks.SessionEnd[0] = func(context.Context, string) { replaced++ }
	if _, err := a.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if inherited != 1 || replaced != 1 {
		t.Fatalf("session end calls: inherited=%d replaced=%d, want 1 each", inherited, replaced)
	}
}

func TestLegacyToolsAreNotExposed(t *testing.T) {
	workspace := newOptionsTestWorkspace(t)

	a := New(workspace, Config{Options: optionsFromEnvironment()}, nil)
	defer a.Close()
	sessionID, err := a.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertTools(t, a, sessionID, []string{"shell", "todo"}, []string{"exec", "exec_session"})
}

func newOptionsTestWorkspace(t *testing.T) *code.Workspace {
	t.Helper()
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	for _, name := range []string{
		"WINGMAN_DISABLE_SHELL",
		"WINGMAN_SANDBOX",
		"WINGMAN_SANDBOX_NETWORK",
		"WINGMAN_DISABLE_WEBSEARCH",
		"WINGMAN_DISABLE_WEBFETCH",
	} {
		t.Setenv(name, "")
	}
	workspace, err := code.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(workspace.Close)
	return workspace
}

func assertTools(t *testing.T, a *Agent, sessionID string, absent, present []string) {
	t.Helper()
	tools := a.Tools(sessionID)
	names := make([]string, 0, len(tools))
	for _, candidate := range tools {
		names = append(names, candidate.Name)
	}
	for _, name := range absent {
		if slices.Contains(names, name) {
			t.Errorf("disabled tool %q is present in %v", name, names)
		}
	}
	for _, name := range present {
		if !slices.Contains(names, name) {
			t.Errorf("enabled tool %q is absent from %v", name, names)
		}
	}
}
