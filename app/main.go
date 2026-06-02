package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/adrianliechti/wingman-agent/server"
)

//go:embed all:public
var publicFS embed.FS

type App struct {
	ctx context.Context

	mu     sync.Mutex
	server *server.Server
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	if s, err := loadSettings(); err == nil {
		s.Apply()
	}
}

func (a *App) GetSettings() (Settings, error) {
	return loadSettings()
}

func (a *App) SaveSettings(s Settings) error {
	current, err := loadSettings()
	if err == nil {
		s.Workspaces = current.Workspaces
	}

	if err := saveSettings(s); err != nil {
		return err
	}

	s.Apply()
	return nil
}

func (a *App) GetWorkspaces() ([]string, error) {
	s, err := loadSettings()
	if err != nil {
		return nil, err
	}

	if len(s.Workspaces) > maxWorkspaces {
		return s.Workspaces[:maxWorkspaces], nil
	}

	return s.Workspaces, nil
}

func (a *App) RemoveWorkspace(path string) ([]string, error) {
	s, err := loadSettings()
	if err != nil {
		return nil, err
	}

	s.RemoveWorkspace(path)

	if err := saveSettings(s); err != nil {
		return nil, err
	}

	return s.Workspaces, nil
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	srv := a.server
	a.mu.Unlock()

	if srv != nil {
		srv.Close()
	}
}

func (a *App) SelectFolder() (string, error) {
	return wailsruntime.OpenDirectoryDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Open Workspace",
	})
}

// OpenWorkspace boots the embedded server on a localhost TCP listener and
// returns its URL. The frontend navigates the webview to that URL, leaving
// the Wails AssetServer (which can't proxy WebSocket upgrades) out of the
// hot path entirely.
func (a *App) OpenWorkspace(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}

	a.mu.Lock()
	if a.server != nil {
		a.mu.Unlock()
		return "", errors.New("workspace already open")
	}
	a.mu.Unlock()

	srv, err := server.New(a.ctx, path, nil)
	if err != nil {
		return "", err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		srv.Close()
		return "", err
	}
	go func() {
		if err := http.Serve(listener, srv); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("server listener: %v", err)
		}
	}()

	a.mu.Lock()
	a.server = srv
	a.mu.Unlock()

	if s, err := loadSettings(); err == nil {
		s.AddWorkspace(path)
		_ = saveSettings(s)
	}

	return fmt.Sprintf("http://%s", listener.Addr().String()), nil
}

func main() {
	// Repair PATH before anything detects agents via exec.LookPath: GUI
	// launches (Finder/Dock) inherit a minimal PATH that hides Homebrew /
	// ~/.local/bin CLIs like codex and copilot.
	ensureShellPath()

	app := &App{}

	startFS, _ := fs.Sub(publicFS, "public")

	opts := &options.App{
		Title: "Wingman Agent",

		Width:  1280,
		Height: 768,

		OnStartup:  app.startup,
		OnShutdown: app.shutdown,

		Bind: []any{app},

		AssetServer: &assetserver.Options{
			Handler: http.FileServer(http.FS(startFS)),
		},
	}

	if err := wails.Run(opts); err != nil {
		panic(err)
	}
}
