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
	"slices"
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
	NoBrowser           bool
	RemoteURL           string
	RemoteToken         string
	disableManagedTools bool
}

// Server serves the web UI and HTTP adapters for one project of its host. The
// server New returns shows the project it started in and fronts a window
// server for each other project that a browser window opens.
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

	front     *Server // the server whose host and preferences a window server shares
	windowsMu sync.Mutex
	windows   map[*Project]*Server // window servers of the front's other projects
}

// New starts a host with a web UI for the project at workDir. Windows may
// open any other project in the home directory too.
func New(ctx context.Context, workDir string, opts *ServerOptions) (*Server, error) {
	if opts == nil {
		opts = new(ServerOptions)
	}
	roots := []string{workDir}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, home)
	}
	h, err := NewHost(ctx, &HostOptions{Roots: roots, disableManagedTools: opts.disableManagedTools})
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
	s.registerRoutes(s.mux)

	csrf := http.NewCrossOriginProtection()
	s.handler = LocalHostOnly(csrf.Handler(s.checkInstance(s.mux)))
	return nil
}

// window is the server of a window that shows a project of the front's host.
func (s *Server) window(p *Project) (*Server, error) {
	if p == s.Project {
		return s, nil
	}
	s.windowsMu.Lock()
	defer s.windowsMu.Unlock()
	if w := s.windows[p]; w != nil {
		return w, nil
	}
	w := &Server{Project: p, noBrowser: true, front: s}
	if err := w.startWindow(); err != nil {
		return nil, err
	}
	if s.windows == nil {
		s.windows = map[*Project]*Server{}
	}
	s.windows[p] = w
	p.windowed.Store(true)
	// A project that closes takes its window server along; a window reopens it.
	p.onClose(func() {
		s.windowsMu.Lock()
		if s.windows[p] == w {
			delete(s.windows, p)
		}
		s.windowsMu.Unlock()
		w.preview.Close()
	})
	return w, nil
}

// windowOf finds the window server whose project has a browser's instance.
func (s *Server) windowOf(instance string) *Server {
	s.windowsMu.Lock()
	defer s.windowsMu.Unlock()
	for p, w := range s.windows {
		if p.scope.InstanceID == instance {
			return w
		}
	}
	return nil
}

// Open opens the project at dir for a window, allowing its directory even
// outside the host's roots, as for a folder the user picked.
func (s *Server) Open(dir string) error {
	s.Project.host.Allow(dir)
	p, err := s.Project.host.projectAt(dir)
	if err != nil {
		return err
	}
	_, err = s.window(p)
	return err
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

// ServeHTTP routes a browser's request to the window server of its project:
// requests carry the instance of the project they were started with, and a
// window opens another project by naming it when it asks for its workspace.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.front == nil {
		query := r.URL.Query()
		if r.URL.Path == "/api/workspace" && query.Has("project") {
			window, err := s.openWindow(query.Get("project"))
			if err != nil {
				http.Error(w, err.Error(), windowStatus(err))
				return
			}
			// Naming a project is discovery, which no instance scopes.
			r = r.Clone(r.Context())
			r.Header.Del(instanceHeader)
			query.Del("instance")
			r.URL.RawQuery = query.Encode()
			window.handler.ServeHTTP(w, r)
			return
		}
		instance := r.Header.Get(instanceHeader)
		if instance == "" {
			instance = query.Get("instance")
		}
		// An instance no window server knows is answered as a replaced workspace.
		if instance != "" && instance != s.Project.scope.InstanceID {
			if window := s.windowOf(instance); window != nil {
				window.handler.ServeHTTP(w, r)
				return
			}
		}
	}
	s.handler.ServeHTTP(w, r)
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
	if !s.Project.host.known(dir) && !recentWorkspace(dir) {
		return nil, &ahp.Error{Code: int(types.ErrorCodePermissionDenied), Message: "open this folder in the desktop app or start a session in it first"}
	}
	p, err := s.Project.host.projectAt(dir)
	if err != nil {
		return nil, err
	}
	return s.window(p)
}

// recentWorkspace reports whether the desktop app opened a folder before.
func recentWorkspace(dir string) bool {
	userSettings, err := settings.Load()
	if err != nil {
		return false
	}
	dir = canonicalPath(dir)
	return slices.ContainsFunc(userSettings.Workspaces, func(path string) bool {
		return canonicalPath(path) == dir
	})
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
		WorkspaceScope:         s.Project.scope,
		Namespace:              s.Project.namespace,
		Protocol:               2,
		Directory:              s.Project.directoryURI(),
		ProjectURI:             projectInfo(s.Project.dir).Uri,
		Name:                   filepath.Base(ws.RootPath),
		Path:                   ws.RootPath,
		Shells:                 append([]terminal.Shell{}, terminal.Shells()...),
		LSP:                    ws.HasLSP(),
		Debug:                  s.Project.debugAvailable(r.Context()),
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
