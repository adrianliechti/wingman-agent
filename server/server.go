package server

import (
	"context"
	"embed"
	"encoding/json"
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
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/dap"
	"github.com/adrianliechti/wingman-agent/pkg/devtools"
	"github.com/adrianliechti/wingman-agent/pkg/filewatch"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/remote"
	"github.com/adrianliechti/wingman-agent/pkg/settings"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"github.com/adrianliechti/wingman-agent/pkg/watch"
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

type Server struct {
	*harness.Config

	noBrowser bool

	workspace *code.Workspace
	telemetry *telemetry.Telemetry

	ctx     context.Context
	cancel  context.CancelFunc
	mux     chi.Router
	handler http.Handler

	closeOnce  sync.Once
	background sync.WaitGroup

	backendsMu sync.Mutex
	backends   map[string]*backendEntry
	scope      WorkspaceScope
	sendMu     sync.Mutex
	ahpOnce    sync.Once
	ahp        *ahpHost

	dapReferenceSeq  atomic.Uint64
	searches         atomic.Int32
	lspDocumentsMu   sync.Mutex
	lspDocuments     map[string]map[*lspConnection]struct{}
	lspRefreshers    map[*lspConnection]struct{} // clients that re-pull diagnostics on request
	lspExternalMu    sync.Mutex
	lspExternalPaths map[string]bool
	fileWriteMu      sync.Mutex

	terminals        *terminal.Manager
	preview          *filePreviewServer
	settingsMu       sync.Mutex
	terminalPosition atomic.Value
	sidebarPosition  atomic.Value

	files           *watch.Monitor
	skillFiles      *watch.Monitor
	prevGit         bool
	prevLSP         bool
	prevDebug       bool
	prevFingerprint uint64
	workspaceFiles  filewatch.Source // feeds language-server file watchers
	fileScanning    atomic.Bool

	managedToolsMu sync.RWMutex
	managedTools   managedToolsStatus
}

func New(ctx context.Context, workDir string, opts *ServerOptions) (_ *Server, err error) {
	if opts == nil {
		opts = new(ServerOptions)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cfg, err := agent.DefaultConfig()
	if err != nil {
		return nil, err
	}
	serverCtx, cancel := context.WithCancel(ctx)
	s := &Server{
		noBrowser: opts.NoBrowser,
		telemetry: cfg.Telemetry,
		ctx:       serverCtx,
		cancel:    cancel,
		backends:  make(map[string]*backendEntry),
	}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	ws, err := code.NewWorkspace(workDir)
	if err != nil {
		return nil, err
	}
	s.workspace = ws
	s.scope = workspaceScope(ws.RootPath)
	s.ahpHost()

	s.terminals = terminal.NewManager(ws.RootPath)
	s.preview, err = newFilePreviewServer(ws.Root)
	if err != nil {
		return nil, err
	}

	cfg.IsolateSessions = true
	wa := agent.New(ws, cfg, nil)
	rt := s.bindBackend(code.BuiltinAgentName, wa)
	s.backends[code.BuiltinAgentName] = &backendEntry{runtime: rt}
	s.Config = cfg.Clone()
	s.RoleModel = wa.RoleModel
	s.Model = func() string {
		option, _ := wa.RoleModel("")
		return option.ID
	}
	s.terminalPosition.Store(settings.WindowTerminalPositionTab)
	s.sidebarPosition.Store(settings.WindowSidebarPositionRight)
	if userSettings, loadErr := settings.Load(); loadErr == nil {
		s.terminalPosition.Store(userSettings.WindowTerminalPosition)
		s.sidebarPosition.Store(userSettings.WindowSidebarPosition)
	}

	ws.WarmUp()
	if terminal.Supported() {
		_ = ws.WithDAPManager(func(manager *dap.Manager) error {
			manager.SetTerminalLauncher(s)
			return nil
		})
	}

	s.prevGit = ws.IsGitRepo()
	s.prevLSP = ws.HasLSP()
	s.prevDebug = s.debugAvailable(serverCtx)
	s.files = watch.New(watch.Options{Active: s.hasClients}, s.checkWorkspace)
	s.workspaceFiles = filewatch.NewScanner(ws.Root.FS(), ".", filewatch.Options{
		Recursive: true,
		Skip:      func(_ string, entry fs.DirEntry) bool { return entry.IsDir() && isExcludedDir(entry.Name()) },
		Track:     func(_ string, entry fs.DirEntry) bool { return !entry.IsDir() },
		Limit:     100000,
	})
	s.background.Go(func() {
		s.files.Run(serverCtx)
	})
	if !opts.disableManagedTools {
		update := ws.StartManagedToolsUpdate(serverCtx, code.ManagedLSPTools, func(progress devtools.Progress) {
			s.setManagedToolsStatus(managedToolsStatus{
				State: "installing", Tool: progress.Tool, Label: progress.Label, Phase: progress.Phase,
				Current: progress.Current, Total: progress.Total,
			})
		})
		s.background.Go(func() {
			changed, updateErr := update.WaitContext(serverCtx)
			if serverCtx.Err() != nil {
				return
			}
			if updateErr != nil {
				fmt.Fprintf(os.Stderr, "managed tools warning: %v\n", updateErr)
				if devtools.IsUnavailable(updateErr) {
					s.setManagedToolsStatus(managedToolsStatus{
						State: "error", Error: updateErr.Error(), Unavailable: devtools.ToolLabels(devtools.UnavailableTools(updateErr)),
					})
				} else {
					s.setManagedToolsStatus(managedToolsStatus{State: "ready"})
				}
			} else {
				s.setManagedToolsStatus(managedToolsStatus{State: "ready"})
			}
			if changed && serverCtx.Err() == nil {
				if s.files != nil {
					s.files.Flush()
				}
			}
		})
	}
	s.skillFiles = watch.New(watch.Options{
		Fallback: 2 * time.Second,
		Active:   s.hasClients,
	}, s.refreshSkills)
	s.background.Go(func() {
		s.skillFiles.Run(serverCtx)
	})

	s.background.Go(func() {
		if err := ws.InitMCP(serverCtx); err != nil && serverCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "MCP init warning: %v\n", err)
		}
	})

	s.background.Go(func() {
		if err := rt.refreshModels(); err != nil && serverCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "Model discovery warning: %v\n", err)
		}
	})

	s.mux = chi.NewRouter()
	s.registerRoutes(s.mux)

	csrf := http.NewCrossOriginProtection()
	s.handler = LocalHostOnly(csrf.Handler(s.checkInstance(s.mux)))
	if opts.RemoteURL != "" {
		if err := s.startRemote(remote.ClientOptions{Relay: opts.RemoteURL, Token: opts.RemoteToken}); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *Server) Close() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.ahp != nil {
			s.ahp.close()
		}

		s.backendsMu.Lock()
		runtimes := make([]*backendRuntime, 0, len(s.backends))
		for _, entry := range s.backends {
			rt := entry.runtime
			if rt == nil {
				continue
			}
			rt.operationMu.Lock()
			rt.closing = true
			rt.operationMu.Unlock()
			runtimes = append(runtimes, rt)
		}
		s.backendsMu.Unlock()
		for _, rt := range runtimes {
			rt.turns.SetHandler(nil)
			rt.turns.Close()
			rt.operations.Wait()
			_ = rt.agent.Close()
		}
		s.background.Wait()
		if s.preview != nil {
			s.preview.Close()
		}
		if s.workspace != nil {
			s.workspace.Close()
		}
		if s.terminals != nil {
			s.terminals.Close()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.telemetry.Shutdown(shutdownCtx)
		cancel()
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) Run(ctx context.Context, port int) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer s.Close()

	listener, err := listenLocal(port)
	if err != nil {
		return err
	}
	defer listener.Close()

	srv := &http.Server{
		Handler: s,
	}

	closeServer := func() { _ = srv.Close() }
	stopOnSignal := context.AfterFunc(ctx, closeServer)
	defer stopOnSignal()
	stopOnClose := context.AfterFunc(s.ctx, closeServer)
	defer stopOnClose()

	url := fmt.Sprintf("http://localhost:%d", listener.Addr().(*net.TCPAddr).Port)
	fmt.Fprintf(os.Stderr, "Wingman running at %s\n", url)
	fmt.Fprintf(os.Stderr, "AHP endpoint: ws://localhost:%d/ahp\n", listener.Addr().(*net.TCPAddr).Port)

	if !s.noBrowser {
		openBrowser(url)
	}

	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) registerRoutes(r chi.Router) {
	s.ahpHost()
	r.Get("/ahp", s.handleAHP)
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

func (s *Server) broadcast(f Frame) {
	f.Session = ""
	s.send(f)
}

const (
	wsWriteTimeout = 5 * time.Second
	wsOutboxBuffer = 256
)

type wsClient struct {
	conn   *websocket.Conn
	outbox chan []byte

	mu     sync.Mutex
	closed bool
}

func newWSClient(conn *websocket.Conn) *wsClient {
	return &wsClient{
		conn:   conn,
		outbox: make(chan []byte, wsOutboxBuffer),
	}
}

func (c *wsClient) enqueue(data []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.outbox <- data:
		return true
	default:
		return false
	}
}

func (c *wsClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.outbox)
}

func (c *wsClient) run() {
	for data := range c.outbox {
		ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
		err := c.conn.Write(ctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			_ = c.conn.CloseNow()

			for range c.outbox {
			}
			return
		}
	}
}

func (s *Server) send(f Frame) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.ahp != nil {
		s.ahp.mu.Lock()
		s.ahp.notifyRoot("x-wingman/workspaceChanged", map[string]any{"channel": "ahp-root://", "event": f})
		s.ahp.mu.Unlock()
	}
}

type workspaceResponse struct {
	WorkspaceScope
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

type managedToolsStatus struct {
	State       string                 `json:"state"`
	Tool        string                 `json:"tool,omitempty"`
	Label       string                 `json:"label,omitempty"`
	Phase       devtools.ProgressPhase `json:"phase,omitempty"`
	Current     int                    `json:"current,omitempty"`
	Total       int                    `json:"total,omitempty"`
	Error       string                 `json:"error,omitempty"`
	Unavailable []string               `json:"unavailable,omitempty"`
}

func (s *Server) setManagedToolsStatus(status managedToolsStatus) {
	s.managedToolsMu.Lock()
	s.managedTools = status
	s.managedToolsMu.Unlock()
	s.broadcast(Frame{Type: EvtWorkspaceChanged})
}

func (s *Server) managedToolsStatus() managedToolsStatus {
	s.managedToolsMu.RLock()
	defer s.managedToolsMu.RUnlock()
	return s.managedTools
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	ws := s.workspace
	info := workspaceResponse{
		WorkspaceScope:         s.scope,
		Protocol:               2,
		Name:                   filepath.Base(ws.RootPath),
		Path:                   ws.RootPath,
		Shells:                 append([]terminal.Shell{}, terminal.Shells()...),
		LSP:                    ws.HasLSP(),
		Debug:                  s.debugAvailable(r.Context()),
		WindowTerminalPosition: s.windowTerminalPosition(),
		WindowSidebarPosition:  s.windowSidebarPosition(),
		Platform:               runtime.GOOS,
	}
	if status := s.managedToolsStatus(); status.State != "" {
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

func (s *Server) debugAvailable(ctx context.Context) bool {
	available := false
	_ = s.workspace.WithDAPManager(func(manager *dap.Manager) error {
		if session := manager.ActiveSession(); session != nil && session.Status().State != dap.StateTerminated {
			available = true
			return nil
		}
		adapters, err := manager.Adapters(ctx)
		available = err == nil && len(adapters) > 0
		return nil
	})
	return available
}

func (s *Server) hasClients() bool {
	if s.ahp == nil {
		return false
	}
	s.ahp.mu.Lock()
	defer s.ahp.mu.Unlock()
	return len(s.ahp.clients) > 0
}

func (s *Server) flushFiles() {
	if s.files != nil {
		s.files.Flush()
	}
	s.broadcast(Frame{Type: EvtFilesChanged})
}

func (s *Server) checkWorkspace() {
	ws := s.workspace

	gitNow := ws.IsGitRepo()
	if gitNow != s.prevGit {
		ws.SyncProjectMode()
		s.broadcast(Frame{Type: EvtWorkspaceChanged})
		if ws.HasLSP() {
			s.refreshDiagnostics()
		}
		s.prevGit = gitNow
	}

	lspNow := ws.HasLSP()
	if lspNow != s.prevLSP {
		s.prevLSP = lspNow
		s.broadcast(Frame{Type: EvtWorkspaceChanged})
		if lspNow {
			s.refreshDiagnostics()
		}
	}

	debugNow := s.debugAvailable(s.ctx)
	if debugNow != s.prevDebug {
		s.prevDebug = debugNow
		s.broadcast(Frame{Type: EvtWorkspaceChanged})
	}

	if ws.HasLSP() {
		s.scanWorkspaceFiles()
	}
	if !ws.HasChanges() {
		s.broadcast(Frame{Type: EvtFilesChanged})
		if ws.HasLSP() {
			s.refreshDiagnostics()
		}
		return
	}
	fp := ws.ChangesFingerprint(s.ctx)
	if fp != s.prevFingerprint {
		s.prevFingerprint = fp
		s.broadcast(Frame{Type: EvtFilesChanged})
		if ws.HasLSP() {
			s.refreshDiagnostics()
		}
	}
}

// Dependency, build and hidden directories are too large or irrelevant to watch.
func isExcludedDir(name string) bool { return strings.HasPrefix(name, ".") || excludedDirs[name] }

// scanWorkspaceFiles passes disk changes to language servers. A slow walk
// or language server must not stall the workspace poller.
func (s *Server) scanWorkspaceFiles() {
	if !s.fileScanning.CompareAndSwap(false, true) {
		return
	}
	s.background.Go(func() {
		defer s.fileScanning.Store(false)
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		defer cancel()
		s.syncWorkspaceFiles(ctx)
	})
}

func (s *Server) syncWorkspaceFiles(ctx context.Context) {
	changes, err := s.workspaceFiles.Changes(ctx)
	if err != nil || len(changes) == 0 {
		return
	}
	events := make([]lsp.FileChange, 0, len(changes))
	for _, change := range changes {
		events = append(events, lsp.FileChange{Path: filepath.Join(s.workspace.RootPath, filepath.FromSlash(change.Path)), Type: lsp.FileChangeType(change.Type)})
	}
	s.workspace.LSPFilesChanged(ctx, events)
}

func (s *Server) refreshSkills() {
	if s.workspace.RefreshSkills() {
		s.broadcast(Frame{Type: EvtSkillsChanged})
	}
}

func convertMessages(messages []harness.Message) []ConversationMessage {
	var result []ConversationMessage
	for _, m := range messages {
		if m.Hidden {
			continue
		}
		cm := ConversationMessage{Role: string(m.Role), InputID: m.InputID}
		for _, c := range m.Content {
			if c.Hidden {
				continue
			}
			cc := ConversationContent{}
			if c.AsText() != "" {
				cc.Text = c.AsText()
				cc.TextID = c.TextID
			}
			if c.File != nil && c.File.Data != "" {
				cc.Image = &ConversationImage{Data: c.File.Data, Name: c.File.Name}
			}
			if c.Reasoning != nil && c.Reasoning.Summary != "" {
				cc.Reasoning = &ConversationReasoning{ID: c.Reasoning.ID, Summary: c.Reasoning.Summary}
			}
			if c.ToolCall != nil {
				display := displayTool(
					c.ToolCall.Name, c.ToolCall.Kind, c.ToolCall.Args, c.ToolCall.Locations,
					c.ToolCall.Presentation,
				)
				cc.ToolCall = &ConversationTool{
					ID:        c.ToolCall.ID,
					Name:      display.name,
					Kind:      display.kind,
					Args:      display.args,
					Locations: display.locations,
					Hint:      display.hint,
				}
			}
			if c.ToolResult != nil {
				display := displayTool(
					c.ToolResult.Name, c.ToolResult.Kind, c.ToolResult.Args, c.ToolResult.Locations,
					c.ToolResult.Presentation,
				)
				cc.ToolResult = &ConversationResult{
					ID:        c.ToolResult.ID,
					Name:      display.name,
					Kind:      display.kind,
					Args:      display.args,
					Locations: display.locations,
					Content:   c.ToolResult.Content,
				}
			}
			cm.Content = append(cm.Content, cc)
		}
		result = append(result, cm)
	}
	return result
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Keep the selected socket bound before announcing the URL or opening a browser.
func listenLocal(port int) (net.Listener, error) {
	if port == 0 {
		if listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", DefaultPort)); err == nil {
			return listener, nil
		}
	}
	return net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
}
