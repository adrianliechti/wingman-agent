package lsp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

type managerTestConn struct {
	jsonrpc2.Conn
	closed chan struct{}
}

func (c *managerTestConn) Close() error {
	close(c.closed)
	return nil
}

var testServer = Server{Name: "test", Command: "test-ls", ExtensionToLanguage: map[string]string{".go": "go"}}

// newTestManager knows one installed server for .go files, ahead of the
// built-in ones, none of which is installed.
func newTestManager(root string) *Manager {
	manager := NewManager(root, testServer)
	manager.lookPath = func(command string) string {
		if command == testServer.Command {
			return "/bin/" + command
		}
		return ""
	}
	return manager
}

func newManagerTestSession(root string) *Session {
	session := &Session{
		server:     testServer,
		root:       root,
		documents:  make(map[string]*document),
		rpc:        protocol.UnimplementedServer{},
		conn:       &managerTestConn{closed: make(chan struct{})},
		cancelFunc: func() {},
	}
	session.alive.Store(true)
	return session
}

func TestServerStartsOnFirstRequestAndIsShared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		manager := newTestManager(root)
		defer manager.Close()
		path := filepath.Join(root, "main.go")
		want := newManagerTestSession(root)
		release := make(chan struct{})
		var starts atomic.Int32
		manager.connect = func(ctx context.Context, dir string, server Server) (*Session, error) {
			starts.Add(1)
			if dir != root || server.executable() != "/bin/test-ls" {
				t.Errorf("connect(%q, %q), want the workspace root and the command found on PATH", dir, server.executable())
			}
			select {
			case <-release:
				return want, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		if manager.FindServer(path) == nil {
			t.Fatal("no server found for a .go file")
		}
		if _, ok := manager.ActiveSession(path); ok || manager.Running() {
			t.Fatal("a server was reported running before any request")
		}
		synctest.Wait()
		if starts.Load() != 0 {
			t.Fatalf("looking a server up started it %d times", starts.Load())
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := manager.GetSession(ctx, path); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled request = %v", err)
		}
		results := make(chan *Session, 8)
		for range cap(results) {
			go func() {
				session, err := manager.GetSession(t.Context(), path)
				if err != nil {
					t.Errorf("GetSession: %v", err)
				}
				results <- session
			}()
		}
		synctest.Wait()
		if session, ok := manager.ActiveSession(path); ok || session != nil {
			t.Fatal("pending startup reported an active session")
		}
		close(release)
		for range cap(results) {
			if got := <-results; got != want {
				t.Fatalf("GetSession = %p, want shared session %p", got, want)
			}
		}
		if starts.Load() != 1 {
			t.Fatalf("shared startup connected %d times, want 1", starts.Load())
		}
		if got, ok := manager.ActiveSession(path); !ok || got != want || !manager.Running() {
			t.Fatalf("ActiveSession = %p, %v; want %p, true", got, ok, want)
		}
	})
}

func TestFailedSessionStartupCanBeRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		manager := newTestManager(root)
		defer manager.Close()
		path := filepath.Join(root, "main.go")
		want := newManagerTestSession(root)
		failure := errors.New("startup failed")
		release := make(chan struct{})
		var starts atomic.Int32
		manager.connect = func(context.Context, string, Server) (*Session, error) {
			if starts.Add(1) == 1 {
				<-release
				return nil, failure
			}
			return want, nil
		}
		results := make(chan error, 2)
		for range cap(results) {
			go func() {
				_, err := manager.GetSession(t.Context(), path)
				results <- err
			}()
		}
		synctest.Wait()
		close(release)
		for range cap(results) {
			if err := <-results; !errors.Is(err, failure) {
				t.Fatalf("shared startup error = %v, want %v", err, failure)
			}
		}
		if got, err := manager.GetSession(t.Context(), path); got != want || err != nil {
			t.Fatalf("retry = %p, %v; want %p, nil", got, err, want)
		}
		if starts.Load() != 2 {
			t.Fatalf("startup attempts = %d, want 2", starts.Load())
		}
	})
}

func TestSessionStartupDiscardsConnectionOfClosedManager(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		manager := newTestManager(root)
		session := newManagerTestSession(root)
		release := make(chan struct{})
		manager.connect = func(context.Context, string, Server) (*Session, error) {
			<-release
			return session, nil
		}
		result := make(chan error, 1)
		go func() {
			_, err := manager.GetSession(t.Context(), filepath.Join(root, "main.go"))
			result <- err
		}()
		synctest.Wait()
		manager.Close()
		close(release)
		if err := <-result; err == nil || !strings.Contains(err.Error(), "manager is closed") {
			t.Fatalf("startup error = %v, want manager is closed", err)
		}
		synctest.Wait()
		if manager.Running() {
			t.Fatal("the stale connection's server is reported running")
		}
		select {
		case <-session.conn.(*managerTestConn).closed:
		default:
			t.Fatal("stale connection was not closed")
		}
	})
}

func TestServerSelection(t *testing.T) {
	root := t.TempDir()
	installed := map[string]string{}
	second := Server{Name: "second", Command: "second-ls", ExtensionToLanguage: map[string]string{".go": "go", ".zig": "zig"}}
	override := Server{Name: "pyright", Command: "my-pyright", ExtensionToLanguage: map[string]string{".py": "python"}}
	manager := NewManager(root, testServer, second, override)
	manager.lookPath = func(command string) string { return installed[command] }
	reset := func(commands ...string) {
		clear(installed)
		for _, command := range commands {
			installed[command] = "/bin/" + command
		}
		clear(manager.commands)
	}

	reset("second-ls", "gopls")
	if _, err := manager.GetSession(t.Context(), filepath.Join(root, "notes.md")); err == nil || !strings.Contains(err.Error(), "no language server is configured for .md files") {
		t.Fatalf("unconfigured extension = %v", err)
	}
	// Each extension has one server: the first registered, installed or not.
	if _, err := manager.GetSession(t.Context(), filepath.Join(root, "main.go")); err == nil || !strings.Contains(err.Error(), `language server test for .go files is not installed: "test-ls" not found on PATH`) {
		t.Fatalf("missing server = %v", err)
	}
	if manager.FindServer(filepath.Join(root, "main.go")) != nil {
		t.Fatal("a server that is not installed was found")
	}
	if server := manager.FindServer(filepath.Join(root, "main.zig")); server == nil || server.Name != "second" {
		t.Fatalf("server = %+v, want the only one for .zig", server)
	}

	reset("test-ls", "second-ls", "gopls")
	if server := manager.FindServer(filepath.Join(root, "main.go")); server == nil || server.Name != "test" || server.executable() != "/bin/test-ls" {
		t.Fatalf("server = %+v, want the first registered one", server)
	}

	reset("my-pyright", "pyright-langserver")
	if server := manager.FindServer(filepath.Join(root, "app.py")); server == nil || server.Command != "my-pyright" {
		t.Fatalf("server = %+v, want the added server to replace the built-in of the same name", server)
	}
	names := map[string]int{}
	for _, server := range manager.servers {
		names[server.Name]++
	}
	if names["pyright"] != 1 || len(names) != len(defaultServers)+2 {
		t.Fatalf("servers = %v", names)
	}
}

func TestManagerOwnsItsServerConfiguration(t *testing.T) {
	config := Server{
		Name: "mock", Command: "mock", ExtensionToLanguage: map[string]string{".mock": "mock"},
		Env: map[string]string{"MODE": "initial"}, Diagnostics: new(true),
	}
	manager := NewManager(t.TempDir(), config)
	defer manager.Close()
	manager.lookPath = func(command string) string { return command }
	clear(config.ExtensionToLanguage)
	config.Env["MODE"] = "altered"
	*config.Diagnostics = false
	first := manager.FindServer("one.mock")
	if first == nil || first.Env["MODE"] != "initial" || !first.ReportsDiagnostics() {
		t.Fatalf("mutating registration changed the manager config: %+v", first)
	}
	clear(first.ExtensionToLanguage)
	*first.Diagnostics = false
	if second := manager.FindServer("two.mock"); second == nil || !second.ReportsDiagnostics() {
		t.Fatalf("mutating lookup changed the next result: %+v", second)
	}
}

func TestInstalledServerIsFoundWithoutWaiting(t *testing.T) {
	root := t.TempDir()
	manager := newTestManager(root)
	installed := false
	manager.lookPath = func(command string) string {
		if installed {
			return "/bin/" + command
		}
		return ""
	}
	path := filepath.Join(root, "main.go")
	if manager.FindServer(path) != nil {
		t.Fatal("a missing server was found")
	}
	installed = true
	if manager.FindServer(path) == nil {
		t.Fatal("the server was still reported missing right after it was installed")
	}
}

func TestParseServersKeepsOrderAndReplacesByName(t *testing.T) {
	servers, err := ParseServers([]byte(`{
		"z-first": {"command": "z", "extensionToLanguage": {".x": "x"}},
		"a-later": {"command": "a", "extensionToLanguage": {".x": "x"}},
		"z-first": {"command": "z2", "extensionToLanguage": {".x": "x"}}
	}`))
	if err != nil || len(servers) != 2 || servers[0].Name != "z-first" || servers[0].Command != "z2" || servers[1].Name != "a-later" {
		t.Fatalf("servers = %+v, %v", servers, err)
	}
	manager := NewManager(t.TempDir(), servers...)
	manager.lookPath = func(command string) string { return "/bin/" + command }
	if server := manager.FindServer("file.x"); server == nil || server.Name != "z-first" {
		t.Fatalf("server = %+v, want the first registered for the extension", server)
	}
}

func TestServerOptions(t *testing.T) {
	no, two := false, 2
	for name, test := range map[string]struct {
		server   Server
		restarts int
	}{
		"default":          {Server{}, defaultMaxRestarts},
		"restartOnCrash":   {Server{RestartOnCrash: &no, MaxRestarts: &two}, 0},
		"maxRestarts":      {Server{MaxRestarts: &two}, 2},
		"zero maxRestarts": {Server{MaxRestarts: new(int)}, 0},
	} {
		if got := test.server.restarts(); got != test.restarts {
			t.Errorf("%s: restarts = %d, want %d", name, got, test.restarts)
		}
	}
	server := Server{StartupTimeout: 1500, ShutdownTimeout: 250, RequestTimeout: 90000}
	if server.startupTimeout().Milliseconds() != 1500 || server.shutdownTimeout().Milliseconds() != 250 || server.requestTimeout().Seconds() != 90 {
		t.Fatalf("timeouts = %v %v %v", server.startupTimeout(), server.shutdownTimeout(), server.requestTimeout())
	}
	if (Server{}).requestTimeout() != defaultRequestTimeout || (Server{}).shutdownTimeout() != defaultShutdownTimeout {
		t.Fatal("unset timeouts did not use the defaults")
	}

	// A server may name its own workspace folder, relative to the workspace.
	root := t.TempDir()
	scoped := testServer
	scoped.WorkspaceFolder = "frontend"
	manager := NewManager(root, scoped)
	manager.lookPath = func(command string) string { return "/bin/" + command }
	defer manager.Close()
	dirs := make(chan string, 1)
	manager.connect = func(_ context.Context, dir string, _ Server) (*Session, error) {
		dirs <- dir
		return newManagerTestSession(dir), nil
	}
	if _, err := manager.GetSession(t.Context(), filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	if dir := <-dirs; dir != filepath.Join(root, "frontend") {
		t.Fatalf("workspace folder = %q", dir)
	}
}

func TestBuiltinServers(t *testing.T) {
	if len(defaultServers) != 12 {
		t.Fatalf("built-in servers = %d, want 12", len(defaultServers))
	}
	for path, want := range map[string]string{
		"main.go": "go", "a.tsx": "typescriptreact", "a.mjs": "javascript", "a.c": "c", "a.C": "cpp", "A.CPP": "cpp",
		"lib.rs": "rust", "App.kt": "kotlin", "x.py": "python", "x.rb": "ruby", "x.swift": "swift", "x.php": "php",
		"x.lua": "lua", "X.java": "java", "x.cs": "csharp",
	} {
		var got []string
		for _, server := range defaultServers {
			if language := server.LanguageIDForPath(path); language != "" {
				got = append(got, language)
			}
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: languages = %v, want [%s]", path, got, want)
		}
	}
}

func TestParseServers(t *testing.T) {
	servers, err := ParseServers([]byte(`{
		"zls": {"command": "zls", "extensionToLanguage": {".zig": "zig"}, "transport": "socket", "restartOnCrash": false,
			"maxRestarts": 1, "diagnostics": false, "shutdownTimeout": 100, "requestTimeout": 200, "workspaceFolder": "zig"},
		"elixir": {"command": "/opt/elixir ls/launch.sh", "args": ["--stdio"], "extensionToLanguage": {".ex": "elixir"},
			"env": {"MIX_ENV": "dev"}, "initializationOptions": {"a": 1}, "settings": {"b": 2}, "startupTimeout": 5000}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Name != "zls" || servers[1].Name != "elixir" {
		t.Fatalf("servers = %+v, want both in the document's order", servers)
	}
	zls, elixir := servers[0], servers[1]
	if elixir.Command != "/opt/elixir ls/launch.sh" || len(elixir.Args) != 1 || elixir.Env["MIX_ENV"] != "dev" || string(elixir.InitializationOptions) != `{"a": 1}` || string(elixir.Settings) != `{"b": 2}` || elixir.startupTimeout().Milliseconds() != 5000 {
		t.Fatalf("elixir = %+v", elixir)
	}
	if zls.startupTimeout() != defaultStartupTimeout || zls.restarts() != 0 || zls.WorkspaceFolder != "zig" || zls.Transport != "socket" {
		t.Fatalf("zls = %+v", zls)
	}

	for name, document := range map[string]string{
		"not an object":      `[]`,
		"trailing data":      `{} {}`,
		"trailing delimiter": `{}]`,
		"missing command":    `{"a": {"extensionToLanguage": {".a": "a"}}}`,
		"command with space": `{"a": {"command": "a --stdio", "extensionToLanguage": {".a": "a"}}}`,
		"missing extensions": `{"a": {"command": "a"}}`,
		"undotted extension": `{"a": {"command": "a", "extensionToLanguage": {"a": "a"}}}`,
		"empty language":     `{"a": {"command": "a", "extensionToLanguage": {".a": ""}}}`,
		"negative timeout":   `{"a": {"command": "a", "extensionToLanguage": {".a": "a"}, "startupTimeout": -1}}`,
		"negative restarts":  `{"a": {"command": "a", "extensionToLanguage": {".a": "a"}, "maxRestarts": -1}}`,
		"unknown transport":  `{"a": {"command": "a", "extensionToLanguage": {".a": "a"}, "transport": "pipe"}}`,
		"unknown field":      `{"a": {"command": "a", "extensionToLanguage": {".a": "a"}, "languages": ["a"]}}`,
	} {
		if _, err := ParseServers([]byte(document)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
