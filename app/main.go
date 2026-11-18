package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/adrianliechti/go-shell"

	"github.com/adrianliechti/wingman-agent/pkg/settings"
	"github.com/adrianliechti/wingman-agent/server"
)

//go:embed all:public
var publicFS embed.FS

// App serves the native windows in this process. Each window names its own
// folder and shares the background host with other windows and processes.
type App struct {
	host         atomic.Pointer[hostClient]
	openMu       sync.Mutex // one open or reconnect at a time
	projects     []string   // folders opened in this process, which a reconnect shows again; guarded by openMu
	reconnecting atomic.Bool

	launcher http.Handler
	// start starts the shared host with a first project; tests run it in process.
	start func(dir string) (hostInfo, error)
	// openWindow opens another window of this process, and startWindow
	// another process of the app where a process has a single window.
	openWindow  func(params map[string]string) error
	startWindow func() error
}

func main() {
	// The background host shares the executable and runs without a window;
	// it inherits the PATH its window repaired.
	if len(os.Args) > 1 && os.Args[1] == hostCommand {
		if err := runHost(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Repair PATH before anything detects agents via exec.LookPath: GUI
	// launches (Finder/Dock) inherit a minimal PATH that hides Homebrew /
	// ~/.local/bin CLIs like codex and copilot.
	ensureShellPath()

	app := &App{start: spawnHost, openWindow: shell.NewWindow, startWindow: startWindowProcess}
	app.launcher = app.newLauncher()

	err := shell.Run(shell.Options{
		Title: "Wingman Agent",

		Handler: app,

		Width:  1280,
		Height: 800,

		MinWidth:  640,
		MinHeight: 400,

		TitleBar: shell.TitleBarOptions{
			Overlay:         true,
			ControlsOffsetX: 6,
			ControlsOffsetY: 4,

			Height: 40,
		},
		// go-shell installs this menu natively on macOS. The Windows title bar
		// renders the same commands in the web UI and owns their Ctrl shortcuts.
		FileMenu: []shell.MenuItem{
			{Title: "New File...", Command: "new-file", Key: "n", Disabled: true},
			{Title: "New Window", Command: "new-window", Key: "n", Shift: true},
			{Title: "Open Folder...", Command: "open-folder", Key: "o"},
			{Separator: true},
			{Title: "Save", Command: "save", Key: "s", Disabled: true},
			{Title: "Save As...", Command: "save-as", Key: "s", Shift: true, Disabled: true},
			{Separator: true},
		},

		Debug: os.Getenv("WINGMAN_DEBUG") != "",
	})

	if err != nil {
		log.Fatal(err)
	}
}

// ServeHTTP hands everything to the shared host once the window opened a
// folder; until then the launcher (start page + its API) answers. Both share
// the window's origin — go-shell opens any other origin in the default
// browser — and the host answers only windows that know its token.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// App-level commands stay available after a workspace is mounted so the
	// native menu can select and switch folders without restarting the shell.
	// A window shows its project by naming it; without one, as a new window
	// opens, it shows the start page.
	if strings.HasPrefix(r.URL.Path, "/app/") || r.URL.Path == "/" && !r.URL.Query().Has("project") {
		a.launcher.ServeHTTP(w, r)
		return
	}

	if host := a.host.Load(); host != nil {
		host.ServeHTTP(w, r)
		return
	}

	a.launcher.ServeHTTP(w, r)
}

func (a *App) newLauncher() http.Handler {
	public, _ := fs.Sub(publicFS, "public")
	files := http.FileServer(http.FS(public))

	mux := http.NewServeMux()
	mux.Handle("/", files)
	// The start page's own files, which stay its once a folder opened.
	mux.Handle("GET /app/", http.StripPrefix("/app", files))

	mux.HandleFunc("GET /app/workspaces", a.handleWorkspaces)
	mux.HandleFunc("POST /app/workspaces/remove", a.handleRemoveWorkspace)
	mux.HandleFunc("POST /app/workspaces/open", a.handleOpenWorkspace)
	mux.HandleFunc("POST /app/folder", a.handleSelectFolder)
	mux.HandleFunc("POST /app/window", a.handleNewWindow)

	return server.LocalHostOnly(http.NewCrossOriginProtection().Handler(mux))
}

func (a *App) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	s, err := settings.Load()

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	workspaces := s.Workspaces

	if len(workspaces) > settings.MaxWorkspaces {
		workspaces = workspaces[:settings.MaxWorkspaces]
	}

	writeJSON(w, workspaces)
}

func (a *App) handleRemoveWorkspace(w http.ResponseWriter, r *http.Request) {
	path, ok := readPath(w, r)

	if !ok {
		return
	}

	s, err := settings.Update(func(current *settings.Settings) {
		current.RemoveWorkspace(path)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, s.Workspaces)
}

func (a *App) handleOpenWorkspace(w http.ResponseWriter, r *http.Request) {
	path, ok := readPath(w, r)
	if !ok {
		return
	}

	// One host serves every window and folder, so the agents of the others
	// keep running; the window then shows the folder by naming it.
	a.openMu.Lock()
	defer a.openMu.Unlock()
	if err := a.attach(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !slices.Contains(a.projects, path) {
		a.projects = append(a.projects, path)
	}

	_, _ = settings.Update(func(settings *settings.Settings) {
		settings.AddWorkspace(path)
	})

	w.WriteHeader(http.StatusNoContent)
}

// handleNewWindow opens another window on the start page. Where a process has
// a single window, it starts another process of the app, which shares the
// host all the same.
func (a *App) handleNewWindow(w http.ResponseWriter, r *http.Request) {
	err := a.openWindow(nil)
	if errors.Is(err, errors.ErrUnsupported) {
		err = a.startWindow()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// startWindowProcess starts another process of the app, with its own window.
func startWindowProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func (a *App) handleSelectFolder(w http.ResponseWriter, r *http.Request) {
	path, err := shell.PickFolder("Open Workspace")

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"path": path})
}

func readPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Path string `json:"path"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}

	if req.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return "", false
	}

	return req.Path, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
