package server

import (
	"context"
	"embed"
	"errors"
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
	"github.com/adrianliechti/wingman-agent/server/ahp"
	"github.com/go-chi/chi/v5"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

//go:embed static/*
var staticFiles embed.FS

var StaticFS, _ = fs.Sub(staticFiles, "static")

// DefaultPort is the preferred port used when no explicit port is requested.
const DefaultPort = 9000

type ServerOptions struct {
	NoBrowser   bool
	RemoteURL   string
	RemoteToken string
	// Projects makes the server show every project of the machine, as the
	// desktop app does: its windows open any folder and its session list holds
	// the sessions of every folder. Without it the server shows the folder it
	// started in and no other.
	Projects            bool
	disableManagedTools bool
}

// Server serves the web UI and HTTP adapters for one project of its host. The
// server New returns shows the project it started in and, when it shows every
// project, fronts a window server for each other project a window opens.
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

	front *Server // the server whose host and preferences a window server shares
}

// New starts a host with a web UI for the project at workDir.
func New(ctx context.Context, workDir string, opts *ServerOptions) (*Server, error) {
	if opts == nil {
		opts = new(ServerOptions)
	}
	hostOptions := &HostOptions{disableManagedTools: opts.disableManagedTools}
	if !opts.Projects {
		hostOptions.folder = workDir
	}
	h, err := NewHost(ctx, hostOptions)
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
	s.terminalPosition.Store(settings.WindowTerminalPositionTab)
	s.sidebarPosition.Store(settings.WindowSidebarPositionRight)
	if userSettings, loadErr := settings.Load(); loadErr == nil {
		s.terminalPosition.Store(userSettings.WindowTerminalPosition)
		s.sidebarPosition.Store(userSettings.WindowSidebarPosition)
	}
	if err := s.startWindow(); err != nil {
		return err
	}
	s.handler = LocalHostOnly(http.NewCrossOriginProtection().Handler(http.HandlerFunc(s.routeWindow)))
	if opts.RemoteURL != "" {
		return s.startRemote(remote.ClientOptions{Relay: opts.RemoteURL, Token: opts.RemoteToken})
	}
	return nil
}

// startWindow starts what a browser window of the server's project uses.
func (s *Server) startWindow() (err error) {
	if s.preview, err = newFilePreviewServer(s.Project.workspace.Root); err != nil {
		return err
	}
	s.mux = chi.NewRouter()
	s.mux.Use(s.checkInstance)
	s.registerRoutes(s.mux)
	return nil
}

// window is the server of a window that shows a project of the front's host.
func (s *Server) window(p *Project) (*Server, error) {
	if p == s.Project {
		return s, nil
	}
	h := p.host
	// Serialize with project teardown, including a close between projectAt and window.
	h.projectMu.Lock()
	defer h.projectMu.Unlock()
	if p.ctx.Err() != nil {
		return nil, errWorkspaceClosed
	}
	if p.window != nil {
		return p.window, nil
	}
	w := &Server{Project: p, noBrowser: true, front: s}
	if err := w.startWindow(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	p.window = w
	h.mu.Unlock()
	return w, nil
}

// windowOf finds the window server whose project has a browser's instance.
func (s *Server) windowOf(instance string) *Server {
	h := s.Project.host
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.projects {
		if p.scope.InstanceID == instance {
			return p.window
		}
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

// routeWindow runs after the host and origin checks. Only initial discovery
// can open a project, and only one the host serves: every folder for the
// desktop app, none but its own for a server of one folder. Later requests
// stay bound to the instance they name.
func (s *Server) routeWindow(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	instance := r.Header.Get(instanceHeader)
	if instance == "" {
		instance = query.Get("instance")
	}
	window := s
	if instance == "" && r.Method == http.MethodGet && r.URL.Path == "/api/workspace" && query.Has("project") {
		var err error
		window, err = s.openWindow(query.Get("project"))
		if err != nil {
			http.Error(w, err.Error(), windowStatus(err))
			return
		}
	} else if instance != "" && instance != s.InstanceID() {
		if other := s.windowOf(instance); other != nil {
			window = other
		}
	}
	window.mux.ServeHTTP(w, r)
}

// openWindow opens the project a window names by its directory or file URI.
func (s *Server) openWindow(project string) (*Server, error) {
	dir := project
	if strings.HasPrefix(project, "file:") {
		path, err := ahp.FilePath(project)
		if err != nil {
			return nil, err
		}
		dir = path
	}
	if !filepath.IsAbs(dir) {
		return nil, ahp.InvalidParams("project must be an absolute directory")
	}
	p, err := s.Project.host.projectAt(dir)
	if err != nil {
		return nil, err
	}
	// A project that is open still after its folder was removed shows in no
	// window.
	if _, err := os.Stat(p.dir); err != nil {
		return nil, err
	}
	return s.window(p)
}

func windowStatus(err error) int {
	var rpc *ahp.Error
	if errors.As(err, &rpc) {
		switch rpc.Code {
		case int(types.ErrorCodePermissionDenied):
			return http.StatusForbidden
		case int(types.ErrorCodeInvalidParams):
			return http.StatusBadRequest
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
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
		r.Get("/workspace/terminals", s.handleTerminals)
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
	Host                   string                          `json:"host"`      // the host process the window connects to
	Directory              string                          `json:"directory"` // the project's file URI, where its sessions work
	ProjectURI             string                          `json:"project"`   // the project's URI as saved sessions name it, with its links resolved
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
		WorkspaceScope: s.Project.scope,
		Namespace:      s.Project.namespace,
		Protocol:       2,
		Directory:      s.Project.directoryURI(),
		ProjectURI:     projectInfo(s.Project.dir).Uri,
		Name:           filepath.Base(ws.RootPath),
		Path:           ws.RootPath,
		Shells:         append([]terminal.Shell{}, terminal.Shells()...),
		// As last found: a window does not wait for a look through the folder.
		LSP:                    s.Project.hasLSP.Load(),
		Debug:                  s.Project.hasDebug.Load(),
		WindowTerminalPosition: s.windowTerminalPosition(),
		WindowSidebarPosition:  s.windowSidebarPosition(),
		Platform:               runtime.GOOS,
	}
	if s.Project.host != nil {
		info.Host = s.Project.host.id
	}
	if status := s.Project.managedToolsStatus(); status.State != "" {
		info.ManagedTools = &status
	}
	writeJSON(w, info)
}

// preferences is the server that holds the browser preferences all windows share.
func (s *Server) preferences() *Server {
	if s.front != nil {
		return s.front
	}
	return s
}

func (s *Server) windowTerminalPosition() settings.WindowTerminalPosition {
	position, ok := s.preferences().terminalPosition.Load().(settings.WindowTerminalPosition)
	if !ok || !position.Valid() {
		return settings.WindowTerminalPositionTab
	}
	return position
}

func (s *Server) windowSidebarPosition() settings.WindowSidebarPosition {
	position, ok := s.preferences().sidebarPosition.Load().(settings.WindowSidebarPosition)
	if !ok || !position.Valid() {
		return settings.WindowSidebarPositionRight
	}
	return position
}
