package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func TestMain(m *testing.M) {
	// The app starts its background host from its own executable; the tests
	// that start a real one do so from the test binary.
	if len(os.Args) > 1 && os.Args[1] == hostCommand {
		if err := runHost(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	os.Exit(m.Run())
}

func jsonString(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// hostsThatStay do not stop by themselves during a test.
var hostsThatStay = hostTiming{grace: time.Hour, quiet: time.Hour}

// testHosts runs shared hosts in the test process instead of background
// processes of the app; it counts the hosts it started.
type testHosts struct {
	timing  hostTiming
	started atomic.Int32
	mu      sync.Mutex
	stops   []func()
}

func newTestHosts(t *testing.T, timing hostTiming) *testHosts {
	hosts := &testHosts{timing: timing}
	t.Cleanup(hosts.stopAll)
	return hosts
}

func (h *testHosts) stopAll() {
	h.mu.Lock()
	stops := h.stops
	h.stops = nil
	h.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
}

func (h *testHosts) start(dir string) (hostInfo, error) {
	h.started.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan hostInfo, 1)
	failed := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := serveHost(ctx, dir, func(info hostInfo) { ready <- info }, h.timing); err != nil {
			failed <- err
		}
	}()
	h.mu.Lock()
	h.stops = append(h.stops, func() { cancel(); <-done })
	h.mu.Unlock()
	select {
	case info := <-ready:
		return info, nil
	case err := <-failed:
		return hostInfo{}, err
	}
}

func newTestApp(hosts *testHosts) *App {
	app := &App{
		start:       hosts.start,
		openWindow:  func(map[string]string) error { return errors.New("no windows in tests") },
		startWindow: func() error { return errors.New("no windows in tests") },
	}
	app.launcher = app.newLauncher()
	return app
}

func serve(app *App, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	return recorder
}

func openFolder(t *testing.T, app *App, dir string) {
	t.Helper()
	if rec := serve(app, "POST", "/app/workspaces/open", `{"path":`+jsonString(t, dir)+`}`); rec.Code != http.StatusNoContent {
		t.Fatalf("open %s: %d %q", dir, rec.Code, rec.Body.String())
	}
}

// windowPath is the project path and host a window shows for a folder.
func windowPath(t *testing.T, app *App, dir string) string {
	t.Helper()
	rec := serve(app, "GET", "/api/workspace?project="+url.QueryEscape(dir), "")
	var window struct {
		Path string `json:"path"`
		Host string `json:"host"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &window) != nil {
		t.Fatalf("window of %s: %d %s", dir, rec.Code, rec.Body.String())
	}
	// A folder may be spelled with or without its links resolved.
	return canonical(t, window.Path) + " " + window.Host
}

func canonical(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// dialAHP connects to the host as the window's page does, through the window.
func dialAHP(t *testing.T, app *App) *websocket.Conn {
	t.Helper()
	window := httptest.NewServer(app)
	t.Cleanup(window.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(window.URL, "http")+"/ahp", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{window.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"channel":"ahp-root://","clientId":"window","protocolVersions":["1.0.0"],"initialSubscriptions":["ahp-root://"]}}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result *struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Result == nil || reply.Result.ProtocolVersion != "1.0.0" {
		t.Fatalf("initialize through the window: %s", data)
	}
	return conn
}

func testGateway(t *testing.T) {
	t.Helper()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(gateway.Close)
	t.Setenv("WINGMAN_URL", gateway.URL)
	t.Setenv("WINGMAN_TOKEN", "test")
}

func TestLauncherSmoke(t *testing.T) {
	testenv.UserHome(t)
	home := testenv.WingmanHome(t)
	t.Setenv("WINGMAN_URL", "https://example.com")
	t.Setenv("WINGMAN_TOKEN", "secret")

	app := newTestApp(newTestHosts(t, hostsThatStay))

	if rec := serve(app, "GET", "/", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Wingman Agent") {
		t.Fatalf("start page: %d", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "--shell-window-drag: drag") {
		t.Fatal("start page has no window drag region")
	} else if !strings.Contains(rec.Body.String(), `data-window-chrome="windows-overlay"`) {
		t.Fatal("start page has no app-owned Windows menu")
	} else if !strings.Contains(rec.Body.String(), "window.shell?.platform") {
		t.Fatal("start page does not consume the injected shell environment")
	} else if strings.Contains(rec.Body.String(), "shell_chrome") {
		t.Fatal("start page still depends on URL-based shell detection")
	}

	if rec := serve(app, "GET", "/app/workspaces", ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("workspaces: %d %q", rec.Code, rec.Body.String())
	}

	if rec := serve(app, "GET", "/app/settings", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("removed settings endpoint: %d %q", rec.Code, rec.Body.String())
	}

	if rec := serve(app, "POST", "/app/workspaces/remove", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("remove without path: %d", rec.Code)
	}

	workspace := t.TempDir()
	openFolder(t, app, workspace)
	config, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if strings.Contains(string(config), "url") || strings.Contains(string(config), "token") || strings.Contains(string(config), "secret") {
		t.Fatalf("config contains connection credentials: %s", config)
	}

	if rec := serve(app, "GET", "/app/workspaces", ""); rec.Code != http.StatusOK {
		t.Fatalf("app commands unavailable after open: %d", rec.Code)
	}
	if rec := serve(app, "GET", "/api/workspace", ""); rec.Code != http.StatusOK {
		t.Fatalf("the host does not serve the window: %d %q", rec.Code, rec.Body.String())
	}

	// Another folder joins the same host; the window shows it by naming it.
	first := app.host.Load()
	openFolder(t, app, workspace)
	other := t.TempDir()
	openFolder(t, app, other)
	if app.host.Load() != first {
		t.Fatal("opening a folder replaced the host and stopped the agents of the first")
	}
	if got := windowPath(t, app, other); !strings.HasPrefix(got, canonical(t, other)+" ") {
		t.Fatalf("the window of the other folder shows %q", got)
	}

	// A window that names its project shows it; one that names none, as a new
	// window opens, shows the start page with its icons.
	if rec := serve(app, "GET", "/?project="+url.QueryEscape(other), ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("the window of a project: %d", rec.Code)
	}
	if rec := serve(app, "GET", "/", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>Wingman Agent</title>") {
		t.Fatalf("a window without a project does not show the start page: %d", rec.Code)
	}
	if rec := serve(app, "GET", "/app/icon_dark.svg", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("start page icon: %d", rec.Code)
	}

	if rec := serve(app, "GET", "/__wingman/host", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("launcher exposed a private host endpoint: %d", rec.Code)
	}
}

func TestLauncherRejectsRebindingAndCrossOriginCommands(t *testing.T) {
	app := &App{}
	handler := app.newLauncher()
	for _, path := range []string{"/", "/app/workspaces", "/app/workspaces/open"} {
		request := httptest.NewRequest("POST", "http://attacker.example"+path, nil)
		request.Header.Set("Origin", "http://attacker.example")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("host check allowed %s: %d", path, response.Code)
		}
	}
	request := httptest.NewRequest("POST", "http://localhost/app/workspaces/open", nil)
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin command allowed: %d", response.Code)
	}
}

// Two windows are two processes of the app; they share one host and so one
// session list, even when they open their first folders at once.
func TestWindowsShareOneHost(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	hosts := newTestHosts(t, hostsThatStay)
	first, second := newTestApp(hosts), newTestApp(hosts)
	shooter, gateway := t.TempDir(), t.TempDir()

	var opening sync.WaitGroup
	opening.Go(func() { openFolder(t, first, shooter) })
	opening.Go(func() { openFolder(t, second, gateway) })
	opening.Wait()

	if started := hosts.started.Load(); started != 1 {
		t.Fatalf("%d hosts started for two windows", started)
	}
	if first.host.Load().info.URL != second.host.Load().info.URL {
		t.Fatal("the windows use different hosts")
	}
	// Each window can show the other's folder, from the same host.
	a, b := windowPath(t, first, gateway), windowPath(t, second, gateway)
	if a != b {
		t.Fatalf("the windows see the folder differently: %q, %q", a, b)
	}
}

// A folder that is gone, such as that of a session a list still shows, does
// not open, and the window opens other folders as before.
func TestWindowOpensFoldersAfterOneThatIsGone(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	hosts := newTestHosts(t, hostsThatStay)
	app := newTestApp(hosts)
	first, second := t.TempDir(), t.TempDir()
	gone := filepath.Join(t.TempDir(), "removed")
	openFolder(t, app, first)

	for _, target := range []string{gone, "file://" + filepath.ToSlash(gone)} {
		if rec := serve(app, "GET", "/api/workspace?project="+url.QueryEscape(target), ""); rec.Code != http.StatusNotFound {
			t.Fatalf("the folder that is gone, as %s: %d %s", target, rec.Code, rec.Body.String())
		}
	}
	if rec := serve(app, "POST", "/app/workspaces/open", `{"path":`+jsonString(t, gone)+`}`); rec.Code == http.StatusNoContent {
		t.Fatal("the launcher opened a folder that is gone")
	}
	// The window's own folder, and another one, open after that.
	if windowPath(t, app, first) == "" || windowPath(t, app, second) == "" {
		t.Fatal("a folder did not open after one that is gone")
	}
	openFolder(t, app, second)
	if started := hosts.started.Load(); started != 1 {
		t.Fatalf("%d hosts started", started)
	}
}

func TestHostAnswersOnlyItsWindows(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	info, err := newTestHosts(t, hostsThatStay).start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong"} {
		request, _ := http.NewRequest(http.MethodGet, info.URL+"/api/workspace", nil)
		if token != "" {
			request.Header.Set(hostTokenHeader, token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("token %q: %d", token, response.StatusCode)
		}
	}
	state, err := readHostInfo()
	if err != nil || state.URL != info.URL || state.PID != os.Getpid() {
		t.Fatalf("the host does not announce itself: %+v %v", state, err)
	}
}

// The page's AHP connection reaches the host through its window, and keeps
// the host from stopping.
func TestWindowCarriesWebSocketsToTheHost(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	app := newTestApp(newTestHosts(t, hostsThatStay))
	openFolder(t, app, t.TempDir())
	conn := dialAHP(t, app)
	client := app.host.Load()
	if err := client.stop(); err == nil {
		t.Fatal("the host stopped while a window follows it")
	}
	_ = conn.CloseNow()
}

func TestUnusedHostStopsAndLeavesNoState(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	done := make(chan error, 1)
	go func() { done <- serveHost(t.Context(), t.TempDir(), nil, hostTiming{grace: 50 * time.Millisecond}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("an unused host kept running")
	}
	if _, err := readHostInfo(); err == nil {
		t.Fatal("a stopped host left its state behind")
	}
}

// A window that finds the host holds the state lock until it opened its
// folder there; the host does not retire meanwhile.
func TestHostWaitsForAWindowThatConnects(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	unlock, err := lockHostState(true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serveHost(t.Context(), t.TempDir(), nil, hostTiming{grace: 20 * time.Millisecond}) }()
	select {
	case <-done:
		unlock()
		t.Fatal("the host retired while a window was connecting")
	case <-time.After(500 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the host did not retire once the window was done")
	}
}

// Native windows share one App. Each folder must reopen on the replacement
// host, even if the most recently picked worktree has disappeared.
func TestWindowReconnectsWhenItsHostStops(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	hosts := newTestHosts(t, hostsThatStay)
	app := newTestApp(hosts)
	first, second, gone := t.TempDir(), t.TempDir(), t.TempDir()
	for _, dir := range []string{first, second, gone} {
		openFolder(t, app, dir)
	}
	lost := app.host.Load()
	hosts.stopAll()
	if err := os.Rename(gone, filepath.Join(t.TempDir(), "moved")); err != nil {
		t.Fatal(err)
	}
	if rec := serve(app, "GET", "/api/workspace", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("a request to the stopped host: %d", rec.Code)
	}
	for deadline := time.Now().Add(30 * time.Second); app.host.Load() == lost; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the window did not reconnect")
		}
	}
	for _, dir := range []string{first, second} {
		if got := windowPath(t, app, dir); !strings.HasPrefix(got, canonical(t, dir)+" ") {
			t.Fatalf("the reconnected window shows %q", got)
		}
	}
}

// After an update, a window stops the unused host of the old build and
// starts its own; a host that a window follows keeps serving every window.
func TestWindowReplacesAnUnusedHostOfAnotherBuild(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	build := selfVersion
	t.Cleanup(func() { selfVersion = build })
	hosts := newTestHosts(t, hostTiming{grace: time.Hour}) // an unused host may be stopped at once

	selfVersion = "old"
	openFolder(t, newTestApp(hosts), t.TempDir())
	selfVersion = "new"
	updated := newTestApp(hosts)
	openFolder(t, updated, t.TempDir())
	if started := hosts.started.Load(); started != 2 {
		t.Fatalf("%d hosts started; the unused host of the old build kept serving", started)
	}
	if version, err := updated.host.Load().version(); err != nil || version != "new" {
		t.Fatalf("the window uses a host of build %q: %v", version, err)
	}

	dialAHP(t, updated)
	selfVersion = "newer"
	other := newTestApp(hosts)
	openFolder(t, other, t.TempDir())
	if hosts.started.Load() != 2 || other.host.Load().info.Token != updated.host.Load().info.Token {
		t.Fatal("a window stopped the host that another window follows")
	}
}

func TestWindowReplacesAnOldHostThatDropsTheStopReply(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	old := httptest.NewUnstartedServer(nil)
	old.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__wingman/host":
			writeJSON(w, map[string]string{"version": "older-build"})
		case "/__wingman/stop":
			_ = old.Listener.Close()
			panic(http.ErrAbortHandler)
		default:
			http.NotFound(w, r)
		}
	})
	old.Start()
	t.Cleanup(old.Close)
	if err := writeHostInfo(hostInfo{URL: old.URL, Token: "old-host"}); err != nil {
		t.Fatal(err)
	}
	hosts := newTestHosts(t, hostsThatStay)
	app := newTestApp(hosts)
	openFolder(t, app, t.TempDir())
	if hosts.started.Load() != 1 || app.host.Load().info.URL == old.URL {
		t.Fatal("the folder did not open on a replacement host")
	}
}

// The app runs its host as a process of its own, which outlives the window;
// when the host dies, the window starts another.
func TestWindowStartsTheHostInTheBackground(t *testing.T) {
	testenv.UserHome(t)
	home := testenv.WingmanHome(t)
	testGateway(t)
	var mu sync.Mutex
	var pids []int
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, pid := range pids {
			if process, err := os.FindProcess(pid); err == nil {
				_ = process.Kill()
			}
		}
	})
	app := &App{start: func(dir string) (hostInfo, error) {
		info, err := spawnHost(dir)
		if err != nil {
			output, _ := os.ReadFile(filepath.Join(home, "app", "host.log"))
			return info, fmt.Errorf("%w\n%s", err, output)
		}
		mu.Lock()
		pids = append(pids, info.PID)
		mu.Unlock()
		return info, nil
	}}
	app.launcher = app.newLauncher()
	dir := t.TempDir()
	openFolder(t, app, dir)
	first := app.host.Load().info
	if first.PID == os.Getpid() {
		t.Fatal("the host runs in the window's process")
	}
	if got := windowPath(t, app, dir); !strings.HasPrefix(got, canonical(t, dir)+" ") {
		t.Fatalf("the background host shows %q", got)
	}

	process, err := os.FindProcess(first.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); app.host.Load().info.PID == first.PID; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the window did not start another host")
		}
		serve(app, "GET", "/api/workspace", "")
	}
	if got := windowPath(t, app, dir); !strings.HasPrefix(got, canonical(t, dir)+" ") {
		t.Fatalf("the new background host shows %q", got)
	}
}

// New Window opens another window of the process on the start page; where a
// process has a single window, it starts another process of the app.
func TestNewWindowOpensTheStartPage(t *testing.T) {
	var opened []map[string]string
	started := 0
	app := &App{
		openWindow: func(params map[string]string) error {
			opened = append(opened, params)
			return nil
		},
		startWindow: func() error {
			started++
			return nil
		},
	}
	app.launcher = app.newLauncher()
	if rec := serve(app, "POST", "/app/window", "{}"); rec.Code != http.StatusNoContent {
		t.Fatalf("new window: %d %q", rec.Code, rec.Body.String())
	}
	if len(opened) != 1 || len(opened[0]) != 0 || started != 0 {
		t.Fatalf("opened %v, started %d processes", opened, started)
	}

	app.openWindow = func(map[string]string) error { return fmt.Errorf("shell: %w", errors.ErrUnsupported) }
	if rec := serve(app, "POST", "/app/window", "{}"); rec.Code != http.StatusNoContent || started != 1 {
		t.Fatalf("new window without windows in the process: %d, started %d processes", rec.Code, started)
	}

	app.startWindow = func() error { return errors.New("no executable") }
	if rec := serve(app, "POST", "/app/window", "{}"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failed new window: %d", rec.Code)
	}
}

// The start page opens a temporary chat on the shared host, starting it when
// none runs. It is not a recent workspace.
func TestStartPageOpensTemporaryChat(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	testGateway(t)
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, t.TempDir())
	}
	app := newTestApp(newTestHosts(t, hostsThatStay))

	if rec := serve(app, "GET", "/", ""); !strings.Contains(rec.Body.String(), `id="open-temporary"`) {
		t.Fatal("start page offers no temporary chat")
	}
	rec := serve(app, "POST", "/app/temporary", "{}")
	var created struct {
		Path string `json:"path"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.Path == "" {
		t.Fatalf("temporary chat: %d %q", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(created.Path); err != nil {
		t.Fatalf("temporary chat has no folder: %v", err)
	}
	window := serve(app, "GET", "/api/workspace?project="+url.QueryEscape(created.Path), "")
	var shown struct {
		Name      string `json:"name"`
		Temporary bool   `json:"temporary"`
	}
	if window.Code != http.StatusOK || json.Unmarshal(window.Body.Bytes(), &shown) != nil || !shown.Temporary || shown.Name != "Temporary Chat" {
		t.Fatalf("temporary chat's window: %d %s", window.Code, window.Body.String())
	}
	if rec := serve(app, "GET", "/app/workspaces", ""); strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("temporary chat became a recent workspace: %s", rec.Body.String())
	}
}
