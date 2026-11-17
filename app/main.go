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
	"strings"
	"sync"
	"sync/atomic"

	"github.com/adrianliechti/go-shell"

	"github.com/adrianliechti/wingman-agent/pkg/settings"
	"github.com/adrianliechti/wingman-agent/server"
)

//go:embed all:public
var publicFS embed.FS

// App is one window. Its page comes from the shared host once the window
// opened a folder, and from the start page until then.
type App struct {
	host         atomic.Pointer[hostClient]
	project      atomic.Pointer[string] // the folder this window opened last
	openMu       sync.Mutex             // one open or reconnect at a time
	reconnecting atomic.Bool

	launcher http.Handler
	// start starts the shared host with a first project; tests run it in process.
	start func(dir string) (hostInfo, error)
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

	app := &App{start: spawnHost}
	app.launcher = app.newLauncher()

	err := shell.Run(shell.Options{
		Title: "Wingman Agent",

		Handler:    app,
		OnShutdown: app.shutdown,

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
	if strings.HasPrefix(r.URL.Path, "/app/") {
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

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(public)))

	mux.HandleFunc("GET /app/workspaces", a.handleWorkspaces)
	mux.HandleFunc("POST /app/workspaces/remove", a.handleRemoveWorkspace)
	mux.HandleFunc("POST /app/workspaces/open", a.handleOpenWorkspace)
	mux.HandleFunc("POST /app/folder", a.handleSelectFolder)

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
	var request struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	// One host serves every window and folder, so the agents of the others
	// keep running; the window then shows the folder by naming it.
	a.openMu.Lock()
	defer a.openMu.Unlock()
	client, err := a.attach(request.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.host.Store(client)
	a.project.Store(&request.Path)

	_, _ = settings.Update(func(settings *settings.Settings) {
		settings.AddWorkspace(request.Path)
	})

	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleSelectFolder(w http.ResponseWriter, r *http.Request) {
	path, err := shell.PickFolder("Open Workspace")

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"path": path})
}

// shutdown closes the window only: the shared host keeps its agents running
// for the other windows, and stops by itself once none follows it.
func (a *App) shutdown() {}

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
