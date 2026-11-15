package server

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/adrianliechti/wingman-agent/pkg/remote"
	"github.com/adrianliechti/wingman-agent/pkg/settings"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"github.com/go-chi/chi/v5"
)

//go:embed static/*
var staticFiles embed.FS

var StaticFS, _ = fs.Sub(staticFiles, "static")

// DefaultPort is the preferred port used when no explicit port is requested.
const DefaultPort = 9000

type ServerOptions struct {
	NoBrowser           bool
	RemoteURL           string
	RemoteToken         string
	disableManagedTools bool
}

// Server serves the web UI and HTTP adapters for its host's default project.
type Server struct {
	Project          *Project
	editor           editorState
	searches         atomic.Int32
	noBrowser        bool
	mux              chi.Router
	handler          http.Handler
	preview          *filePreviewServer
	settingsMu       sync.Mutex
	terminalPosition atomic.Value
	sidebarPosition  atomic.Value
}

// New starts a host with a web UI for the project at workDir.
func New(ctx context.Context, workDir string, opts *ServerOptions) (*Server, error) {
	if opts == nil {
		opts = new(ServerOptions)
	}
	h, err := NewHost(ctx, &HostOptions{Roots: []string{workDir}, disableManagedTools: opts.disableManagedTools})
	if err != nil {
		return nil, err
	}
	p, err := h.newProject(workDir)
	if err != nil {
		h.Close()
		return nil, err
	}
	h.show(p)
	s := &Server{Project: p, noBrowser: opts.NoBrowser}
	if err := s.startWeb(opts); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// startWeb adds what only the web UI uses; a host's projects go without it.
func (s *Server) startWeb(opts *ServerOptions) (err error) {
	if s.preview, err = newFilePreviewServer(s.Project.workspace.Root); err != nil {
		return err
	}
	s.terminalPosition.Store(settings.WindowTerminalPositionTab)
	s.sidebarPosition.Store(settings.WindowSidebarPositionRight)
	if userSettings, loadErr := settings.Load(); loadErr == nil {
		s.terminalPosition.Store(userSettings.WindowTerminalPosition)
		s.sidebarPosition.Store(userSettings.WindowSidebarPosition)
	}

	s.mux = chi.NewRouter()
	s.registerRoutes(s.mux)

	csrf := http.NewCrossOriginProtection()
	s.handler = LocalHostOnly(csrf.Handler(s.checkInstance(s.mux)))
	if opts.RemoteURL != "" {
		return s.startRemote(remote.ClientOptions{Relay: opts.RemoteURL, Token: opts.RemoteToken})
	}
	return nil
}

// Close stops the web adapter and the host it owns.
func (s *Server) Close() {
	if s.preview != nil {
		s.preview.Close()
	}
	if s.Project == nil {
		return
	}
	if s.Project.host != nil {
		s.Project.host.Close()
	} else {
		s.Project.close()
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) InstanceID() string { return s.Project.InstanceID() }

func (s *Server) Run(ctx context.Context, port int) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer s.Close()

	listeners, err := listenLocal(port, DefaultPort)
	if err != nil {
		return err
	}
	port = listeners[0].Addr().(*net.TCPAddr).Port

	url := fmt.Sprintf("http://localhost:%d", port)
	fmt.Fprintf(os.Stderr, "Wingman running at %s\n", url)
	fmt.Fprintf(os.Stderr, "AHP endpoint: ws://localhost:%d/ahp\n", port)

	if !s.noBrowser {
		openBrowser(url)
	}
	return serveLocal(s, listeners, ctx, s.Project.ctx)
}

func (s *Server) registerRoutes(r chi.Router) {
	s.Project.host.mu.Lock()
	s.Project.onDiagnostics = s.refreshDiagnostics
	s.Project.host.mu.Unlock()

	r.Get("/ahp", s.Project.host.handleAHP)
	r.Get("/lsp", s.handleLSP)
	r.Get("/dap", s.handleDAP)
	r.Get("/content", s.handleFileDownload)
	r.Get("/preview", s.handleFilePreview)
	r.Route("/api", func(r chi.Router) {

		s.registerSessionRoutes(r)

		// Workspace setup and external source retrieval are host services; editor
		// and debugger operations use the LSP and DAP WebSocket gateways.
		r.Post("/workspace/debug/targets", s.handleDebugTargets)
		r.Post("/workspace/debug/plan", s.handleDebugPlan)
		r.Post("/settings/window.terminal.position", s.handleWindowTerminalSettings)
		r.Post("/settings/window.sidebar.position", s.handleWindowSidebarSettings)
		r.Get("/workspace/skills", s.handleSkills)
		r.Get("/workspace/activity", s.handleWorkspaceActivity)
		r.Get("/workspace/files", s.handleFileSearch)
		r.Post("/workspace/search", s.handleWorkspaceSearch)
		r.Post("/workspace/reveal", s.handleReveal)
		r.Get("/workspace/terminals/{terminal}/status", s.handleTerminalStatus)
		r.Get("/workspace", s.handleWorkspace)
		r.Get("/workspace/insights", s.handleWorkspaceInsights)
		r.Route("/workspace/git", func(r chi.Router) {
			r.Get("/", s.handleWorkspaceGit)
			r.Get("/commits", s.handleGitCommits)
			r.Post("/commits", s.handleGitAction("commit"))
			r.Get("/branches", s.handleGitBranches)
			r.Post("/branches", s.handleGitAction("createBranch"))
			r.Get("/diff", s.handleGitDiff)
			r.Post("/checkout", s.handleGitAction("checkout"))
			r.Post("/fetch", s.handleGitFetch)
			r.Post("/pull", s.handleGitAction("pull"))
			r.Post("/push", s.handleGitAction("push"))
			r.Post("/init", s.handleGitAction("init"))
		})
	})

	fileServer := http.FileServer(http.FS(StaticFS))
	r.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if p := strings.Trim(path.Clean(req.URL.Path), "/"); p != "" {
			if _, err := fs.Stat(StaticFS, p); err != nil {
				req.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, req)
	}))
}

type workspaceResponse struct {
	WorkspaceScope
	Namespace              string                          `json:"namespace"` // of the project's channels
	Protocol               int                             `json:"protocol"`
	Name                   string                          `json:"name"`
	Path                   string                          `json:"path"`
	Shells                 []terminal.Shell                `json:"shells"`
	LSP                    bool                            `json:"lsp"`
	Debug                  bool                            `json:"debug"`
	WindowTerminalPosition settings.WindowTerminalPosition `json:"window.terminal.position"`
	WindowSidebarPosition  settings.WindowSidebarPosition  `json:"window.sidebar.position"`
	Platform               string                          `json:"platform"`
	ManagedTools           *managedToolsStatus             `json:"managed_tools,omitempty"`
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	ws := s.Project.workspace
	info := workspaceResponse{
		WorkspaceScope:         s.Project.scope,
		Namespace:              s.Project.namespace,
		Protocol:               2,
		Name:                   filepath.Base(ws.RootPath),
		Path:                   ws.RootPath,
		Shells:                 append([]terminal.Shell{}, terminal.Shells()...),
		LSP:                    ws.HasLSP(),
		Debug:                  s.Project.debugAvailable(r.Context()),
		WindowTerminalPosition: s.windowTerminalPosition(),
		WindowSidebarPosition:  s.windowSidebarPosition(),
		Platform:               runtime.GOOS,
	}
	if status := s.Project.managedToolsStatus(); status.State != "" {
		info.ManagedTools = &status
	}
	writeJSON(w, info)
}

func (s *Server) windowTerminalPosition() settings.WindowTerminalPosition {
	position, ok := s.terminalPosition.Load().(settings.WindowTerminalPosition)
	if !ok || !position.Valid() {
		return settings.WindowTerminalPositionTab
	}
	return position
}

func (s *Server) windowSidebarPosition() settings.WindowSidebarPosition {
	position, ok := s.sidebarPosition.Load().(settings.WindowSidebarPosition)
	if !ok || !position.Valid() {
		return settings.WindowSidebarPositionRight
	}
	return position
}
