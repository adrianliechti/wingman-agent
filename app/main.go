package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/go-shell"

	"github.com/adrianliechti/wingman-agent/pkg/settings"
	"github.com/adrianliechti/wingman-agent/server"
)

//go:embed all:public
var publicFS embed.FS

type App struct {
	server atomic.Pointer[server.Server]

	launcher http.Handler
}

const shutdownTimeout = time.Second

func main() {
	// Repair PATH before anything detects agents via exec.LookPath: GUI
	// launches (Finder/Dock) inherit a minimal PATH that hides Homebrew /
	// ~/.local/bin CLIs like codex and copilot.
	ensureShellPath()

	app := &App{}
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

// ServeHTTP hands everything to the workspace server once one is open;
// until then the launcher (start page + its API) answers. Both share the
// window's origin — go-shell opens any other origin in the default browser,
// and staying behind its session cookie keeps the workspace server
// unreachable for other local processes.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// App-level commands stay available after a workspace is mounted so the
	// native menu can select and switch folders without restarting the shell.
	if strings.HasPrefix(r.URL.Path, "/app/") {
		a.launcher.ServeHTTP(w, r)
		return
	}

	if srv := a.server.Load(); srv != nil {
		srv.ServeHTTP(w, r)
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
		Path    string `json:"path"`
		Replace bool   `json:"replace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	current := a.server.Load()
	if current != nil && request.Replace && r.Header.Get("X-Wingman-Instance") != current.InstanceID() {
		http.Error(w, "workspace instance changed; reload the workspace", http.StatusConflict)
		return
	}
	if current != nil && !request.Replace {
		http.Error(w, "workspace already open", http.StatusConflict)
		return
	}
	srv, err := server.New(context.Background(), request.Path, nil)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !a.server.CompareAndSwap(current, srv) {
		srv.Close()
		http.Error(w, "workspace changed while opening", http.StatusConflict)
		return
	}

	_, _ = settings.Update(func(settings *settings.Settings) {
		settings.AddWorkspace(request.Path)
	})

	w.WriteHeader(http.StatusNoContent)

	if current != nil {
		go current.Close()
	}
}

func (a *App) handleSelectFolder(w http.ResponseWriter, r *http.Request) {
	path, err := shell.PickFolder("Open Workspace")

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"path": path})
}

// shutdown gives normal cleanup a brief opportunity to finish without making
// native app termination depend on a slow protocol handshake or installer.
func (a *App) shutdown() {
	srv := a.server.Load()
	if srv == nil {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Close()
	}()

	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		log.Println("shutdown timed out, exiting anyway")
	}
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
