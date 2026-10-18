package agent

import (
	"context"
	"slices"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	wingmanmcp "github.com/adrianliechti/wingman-agent/pkg/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

func TestResolveOptionsFromEnvironment(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("WINGMAN_DISABLE_SHELL", value)
			t.Setenv("WINGMAN_SANDBOX_NETWORK", value)
			resolved := resolveOptions(nil)
			if !resolved.DisableShell || resolved.Sandbox == nil || resolved.Sandbox.Network == nil || !*resolved.Sandbox.Network {
				t.Fatalf("WINGMAN_DISABLE_SHELL/WINGMAN_SANDBOX_NETWORK=%q did not enable options: %+v", value, resolved)
			}
		})
	}

	for _, value := range []string{"", "0", "false", "no", "off", "anything"} {
		t.Run("ignored_"+value, func(t *testing.T) {
			t.Setenv("WINGMAN_DISABLE_SHELL", value)
			t.Setenv("WINGMAN_SANDBOX_NETWORK", value)
			resolved := resolveOptions(nil)
			if resolved.DisableShell || resolved.Sandbox == nil || resolved.Sandbox.Network == nil || *resolved.Sandbox.Network {
				t.Fatalf("WINGMAN_DISABLE_SHELL/WINGMAN_SANDBOX_NETWORK=%q enabled options: %+v", value, resolved)
			}
		})
	}

	for _, value := range []string{"off", "false", "0", "no", "disabled", " OFF "} {
		t.Run("sandbox_off_"+value, func(t *testing.T) {
			t.Setenv("WINGMAN_SANDBOX", value)
			if !resolveOptions(nil).DisableSandbox {
				t.Fatalf("WINGMAN_SANDBOX=%q did not disable sandbox", value)
			}
		})
	}

	for _, value := range []string{"", "workspace", "workspace-write", "WORKSPACE", "on", "true", "1", "typo"} {
		t.Run("sandbox_on_"+value, func(t *testing.T) {
			t.Setenv("WINGMAN_SANDBOX", value)
			if resolveOptions(nil).DisableSandbox {
				t.Fatalf("WINGMAN_SANDBOX=%q disabled sandbox", value)
			}
		})
	}

	t.Setenv("WINGMAN_DISABLE_SHELL", "")
	t.Setenv("WINGMAN_SANDBOX", "off")
	t.Setenv("WINGMAN_SANDBOX_NETWORK", "on")
	t.Setenv("WINGMAN_DISABLE_WEBSEARCH", "yes")
	t.Setenv("WINGMAN_DISABLE_WEBFETCH", "on")
	resolved := resolveOptions(nil)
	if !resolved.DisableWebSearch || !resolved.DisableWebFetch || !resolved.DisableSandbox || !*resolved.Sandbox.Network {
		t.Fatalf("resolved options = %+v", resolved)
	}

	overridden := resolveOptions([]Options{{
		Sandbox: &SandboxOptions{
			Network: new(false),
		},
	}})
	if *overridden.Sandbox.Network {
		t.Fatalf("explicit SandboxOptions did not override environment: %+v", overridden.Sandbox)
	}
}

func TestResolveOptionsIgnoresSeparatedWebEnvironmentNames(t *testing.T) {
	t.Setenv("WINGMAN_DISABLE_WEBSEARCH", "")
	t.Setenv("WINGMAN_DISABLE_WEBFETCH", "")
	t.Setenv("WINGMAN_DISABLE_WEB_SEARCH", "1")
	t.Setenv("WINGMAN_DISABLE_WEB_FETCH", "1")

	resolved := resolveOptions(nil)
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
			a := New(workspace, &harness.Config{}, nil, test.options)
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

	a := New(workspace, &harness.Config{}, nil)
	defer a.Close()

	// The environment is a startup input, not mutable per-session state.
	t.Setenv("WINGMAN_DISABLE_SHELL", "")
	t.Setenv("WINGMAN_DISABLE_WEBSEARCH", "")
	t.Setenv("WINGMAN_DISABLE_WEBFETCH", "")

	sessionID, err := a.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertTools(t, a, sessionID,
		[]string{"shell", "exec", "exec_session", "search_web", "fetch", "todo"},
		[]string{"read", "edit"},
	)
}

func TestTelemetryOptionOverridesHarnessConfig(t *testing.T) {
	workspace := newOptionsTestWorkspace(t)
	inherited := &telemetry.Telemetry{}
	override := &telemetry.Telemetry{}
	a := New(workspace, &harness.Config{Telemetry: inherited}, nil, Options{Telemetry: override})
	defer a.Close()
	sessionID, err := a.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state := a.session(sessionID)
	if state == nil {
		t.Fatal("session state was not created")
	}
	if state.aa.Telemetry != override {
		t.Fatalf("session telemetry = %p, want explicit override %p", state.aa.Telemetry, override)
	}
}

func TestCloseWithOwnedTelemetryClosesWorkspaceMCPSessions(t *testing.T) {
	workspace := newOptionsTestWorkspace(t)
	manager := wingmanmcp.NewManager(&wingmanmcp.Config{Servers: map[string]wingmanmcp.ServerConfig{}})
	workspace.MCP = manager

	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.AddSession("test", clientSession)

	a := New(workspace, &harness.Config{Telemetry: &telemetry.Telemetry{}}, nil, Options{
		ShutdownTelemetryOnClose: true,
	})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	waited := make(chan error, 1)
	go func() { waited <- clientSession.Wait() }()
	select {
	case <-waited:
	case <-ctx.Done():
		t.Fatal("owned telemetry shutdown left the MCP session open")
	}
}

func TestLegacyToolsAreNotExposed(t *testing.T) {
	workspace := newOptionsTestWorkspace(t)

	a := New(workspace, &harness.Config{}, nil)
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
