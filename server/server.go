package server

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	coder "github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/system"
)

// Compile-time check: the HTTP server is the [code.UI] that any
// in-process coder.Agent delegates ask_user / shell-confirm to.
var _ code.UI = (*Server)(nil)

//go:embed static/*
var staticFiles embed.FS

var StaticFS, _ = fs.Sub(staticFiles, "static")

type ServerOptions struct {
	Port      int
	NoBrowser bool
}

type Server struct {
	port      int
	noBrowser bool

	workspace *code.Workspace
	config    *agent.Config

	// ctx lives for the lifetime of the server. Agent turns and background
	// goroutines tie their cancellation to this — NOT to any HTTP request
	// ctx. Tying a Send to r.Context() would cancel the agent mid-turn on
	// a WS disconnect/reconnect.
	ctx     context.Context
	mux     *http.ServeMux
	handler http.Handler

	mu    sync.Mutex
	agent code.Agent // active backend (wingman by default; swapped on /api/agent)

	// phases tracks per-session UI phase (idle/thinking/streaming/tool_running).
	// Lives at the server because phase is computed from streamed events —
	// the agent only knows about messages.
	phasesMu sync.Mutex
	phases   map[string]string

	wsMu    sync.Mutex
	wsConns map[*websocket.Conn]*wsClient

	// pendingPrompts maps prompt id → server-side bookkeeping for an
	// outstanding Ask/Confirm. The WS read loop drains
	// prompt_response messages into the right channel; sendSessionState
	// replays the rest on reconnect.
	promptsMu      sync.Mutex
	pendingPrompts map[string]pendingPrompt
}

func New(ctx context.Context, workDir string, opts *ServerOptions) (*Server, error) {
	if opts == nil {
		opts = new(ServerOptions)
	}

	cfg, err := agent.DefaultConfig()
	if err != nil {
		return nil, err
	}
	ws, err := code.NewWorkspace(workDir)
	if err != nil {
		return nil, err
	}

	s := &Server{
		port:           opts.Port,
		noBrowser:      opts.NoBrowser,
		workspace:      ws,
		config:         cfg,
		ctx:            ctx,
		phases:         map[string]string{},
		wsConns:        map[*websocket.Conn]*wsClient{},
		pendingPrompts: map[string]pendingPrompt{},
	}

	// Default to the wingman in-process agent; user can swap via
	// POST /api/agent.
	wa := coder.New(ws, cfg, nil)
	wa.SetUI(s)
	s.agent = wa

	ws.WarmUp()
	go func() {
		if err := ws.InitMCP(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "MCP init warning: %v\n", err)
		}
	}()

	// Populate the wingman agent's upstream model cache + pick a default.
	go func() {
		if w, ok := s.agent.(*coder.Agent); ok {
			w.AutoSelectModel(ctx)
		}
	}()

	s.mux = http.NewServeMux()
	s.registerRoutes(s.mux)

	csrf := http.NewCrossOriginProtection()
	s.handler = csrf.Handler(s.mux)

	return s, nil
}

func (s *Server) Close() {
	// Tear down the active backend (kills ACP subprocess if any), then
	// the shared workspace.
	s.mu.Lock()
	a := s.agent
	s.agent = nil
	s.mu.Unlock()
	if a != nil {
		_ = a.Close()
	}
	s.workspace.Close()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// activeAgent returns the currently selected backend under the agent mu.
// Callers should not hold the mu while doing IO with the agent (the
// agent's own internal locks already protect it; we just need a stable
// snapshot of the pointer).
func (s *Server) activeAgent() code.Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agent
}

// swapAgent atomically replaces the active backend. Closes the prior
// one outside the lock to avoid holding mu across IO.
func (s *Server) swapAgent(next code.Agent) {
	s.mu.Lock()
	prev := s.agent
	s.agent = next
	s.mu.Unlock()
	if prev != nil && prev != next {
		_ = prev.Close()
	}
}

func (s *Server) handleWebSocketURL(w http.ResponseWriter, r *http.Request) {
	proto := "ws"
	if r.TLS != nil {
		proto = "wss"
	}
	writeJSON(w, map[string]string{"url": fmt.Sprintf("%s://%s/ws", proto, r.Host)})
}

func (s *Server) Run(ctx context.Context) error {
	// Adopt the caller-supplied ctx as the server-lifetime ctx so the
	// signal handler below tears down everything that lives off s.ctx
	// (pollFiles, agent turns started during HTTP handlers, etc.).
	// Previously these two were independent — pollFiles would outlive
	// Run if only the local derived ctx was cancelled.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.ctx = ctx

	// Started here (not in New) so SIGTERM via the handler below stops it.
	go s.pollFiles(ctx)

	port, err := system.FreePort(s.port)
	if err != nil {
		return err
	}
	s.port = port

	srv := &http.Server{
		Addr:    fmt.Sprintf("localhost:%d", s.port),
		Handler: s,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
		srv.Close()
	}()

	url := fmt.Sprintf("http://localhost:%d", s.port)
	fmt.Fprintf(os.Stderr, "Wingman running at %s\n", url)

	if !s.noBrowser {
		openBrowser(url)
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/files", s.handleFiles)
	mux.HandleFunc("GET /api/files/read", s.handleFileRead)
	mux.HandleFunc("GET /api/files/search", s.handleFilesSearch)
	mux.HandleFunc("GET /api/files/download", s.handleFileDownload)
	mux.HandleFunc("DELETE /api/files", s.handleFileDelete)
	mux.HandleFunc("POST /api/files/rename", s.handleFileRename)
	mux.HandleFunc("POST /api/files/copy", s.handleFileCopy)
	mux.HandleFunc("POST /api/files/write", s.handleFileWrite)
	mux.HandleFunc("GET /api/diffs", s.handleDiffs)
	mux.HandleFunc("POST /api/diffs/revert", s.handleDiffRevert)
	mux.HandleFunc("GET /api/checkpoints", s.handleCheckpoints)
	mux.HandleFunc("POST /api/checkpoints/{hash}/restore", s.handleCheckpointRestore)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("POST /api/sessions", s.handleNewSession)
	mux.HandleFunc("POST /api/sessions/{id}/load", s.handleLoadSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /api/model", s.handleModel)
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("POST /api/model", s.handleSetModel)
	mux.HandleFunc("GET /api/effort", s.handleEffort)
	mux.HandleFunc("POST /api/effort", s.handleSetEffort)
	mux.HandleFunc("GET /api/agents", s.handleAgents)
	mux.HandleFunc("GET /api/agent", s.handleAgent)
	mux.HandleFunc("POST /api/agent", s.handleSetAgent)
	mux.HandleFunc("GET /api/mode", s.handleMode)
	mux.HandleFunc("POST /api/mode", s.handleSetMode)
	mux.HandleFunc("GET /api/diagnostics", s.handleDiagnostics)
	mux.HandleFunc("GET /api/skills", s.handleSkills)
	mux.HandleFunc("GET /api/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /api/ws", s.handleWebSocketURL)

	mux.HandleFunc("/ws", s.handleWebSocket)

	staticFS, _ := fs.Sub(staticFiles, "static")
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/", fileServer)
}

// ─── Phase tracking ───────────────────────────────────────────────

func (s *Server) sessionPhase(id string) string {
	s.phasesMu.Lock()
	defer s.phasesMu.Unlock()
	if p := s.phases[id]; p != "" {
		return p
	}
	return "idle"
}

func (s *Server) setSessionPhase(id, phase string) {
	s.phasesMu.Lock()
	if s.phases[id] == phase {
		s.phasesMu.Unlock()
		return
	}
	if phase == "" || phase == "idle" {
		delete(s.phases, id)
	} else {
		s.phases[id] = phase
	}
	s.phasesMu.Unlock()
	s.sendSession(id, Frame{Type: EvtPhase, Phase: phase})
}

// ─── Frame send helpers ───────────────────────────────────────────

func (s *Server) sendSession(sid string, f Frame) {
	f.Session = sid
	s.send(f)
}

func (s *Server) broadcast(f Frame) {
	f.Session = ""
	s.send(f)
}

const (
	wsWriteTimeout = 5 * time.Second
	wsOutboxBuffer = 256
)

// wsClient serializes writes for a single connection so frame order is
// preserved (phase → text_delta → usage → phase, tool_call → tool_result).
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
			// Drain so enqueue stays non-blocking until close() runs.
			for range c.outbox {
			}
			return
		}
	}
}

func (s *Server) send(f Frame) {
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	s.wsMu.Lock()
	clients := make([]*wsClient, 0, len(s.wsConns))
	for _, c := range s.wsConns {
		clients = append(clients, c)
	}
	s.wsMu.Unlock()
	for _, c := range clients {
		if !c.enqueue(data) {
			_ = c.conn.CloseNow()
		}
	}
}

// sendSessionState pushes the full transcript snapshot for a session,
// used after LoadSession. Any prompts the agent is still waiting on are
// replayed so a load mid-elicit doesn't leave the user staring at a
// frozen turn.
func (s *Server) sendSessionState(sid string) {
	a := s.activeAgent()
	if a == nil {
		return
	}
	messages := a.Messages(sid)
	u := a.Usage(sid)
	s.sendSession(sid, Frame{
		Type:         EvtSessionState,
		Phase:        s.sessionPhase(sid),
		Messages:     convertMessages(messages),
		InputTokens:  u.InputTokens,
		CachedTokens: u.CachedTokens,
		OutputTokens: u.OutputTokens,
	})
	for _, f := range s.pendingPromptFramesFor(sid) {
		s.send(f)
	}
}

// ─── Session endpoints ────────────────────────────────────────────

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	a := s.activeAgent()
	if a == nil {
		writeJSON(w, []SessionEntry{})
		return
	}
	infos, err := a.ListSessions(r.Context())
	if err != nil {
		// Surface to the parent log — sidebar still shows "No sessions
		// yet", but the developer can see whether the ACP server
		// rejected the call vs genuinely returned empty.
		fmt.Fprintf(os.Stderr, "list sessions (%s): %v\n", a.Name(), err)
		writeJSON(w, []SessionEntry{})
		return
	}
	out := make([]SessionEntry, 0, len(infos))
	for _, si := range infos {
		ent := SessionEntry{ID: si.ID, Title: si.Title}
		if !si.UpdatedAt.IsZero() {
			ent.UpdatedAt = si.UpdatedAt.Format(time.RFC3339)
			ent.CreatedAt = ent.UpdatedAt
		}
		out = append(out, ent)
	}
	slices.SortFunc(out, func(a, b SessionEntry) int {
		return cmp.Compare(b.UpdatedAt, a.UpdatedAt)
	})
	writeJSON(w, out)
}

func (s *Server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	a := s.activeAgent()
	if a == nil {
		http.Error(w, "no active agent", http.StatusInternalServerError)
		return
	}
	id, err := a.NewSession(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"id": id})
}

func (s *Server) handleLoadSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	a := s.activeAgent()
	if a == nil {
		http.Error(w, "no active agent", http.StatusInternalServerError)
		return
	}
	// s.ctx (not r.Context()) so a WS reconnect mid-load doesn't abort.
	if err := a.LoadSession(s.ctx, id); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			http.Error(w, "load not supported for this agent", http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.sendSessionState(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	a := s.activeAgent()
	if a == nil {
		http.Error(w, "no active agent", http.StatusInternalServerError)
		return
	}
	if err := a.DeleteSession(r.Context(), id); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			http.Error(w, "delete not supported for this agent", http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.phasesMu.Lock()
	delete(s.phases, id)
	s.phasesMu.Unlock()
	s.broadcast(Frame{Type: EvtSessionsChanged})
	w.WriteHeader(http.StatusNoContent)
}

// ─── Model / Effort endpoints ─────────────────────────────────────

func (s *Server) handleModel(w http.ResponseWriter, _ *http.Request) {
	a := s.activeAgent()
	if a == nil {
		writeJSON(w, map[string]string{"model": ""})
		return
	}
	_, current := a.Models()
	writeJSON(w, map[string]string{"model": current})
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	a := s.activeAgent()
	if a == nil {
		writeJSON(w, []map[string]string{})
		return
	}
	available, _ := a.Models()
	result := make([]map[string]string, 0, len(available))
	for _, m := range available {
		result = append(result, map[string]string{"id": m.ID, "name": m.Name})
	}
	writeJSON(w, result)
}

func (s *Server) handleSetModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}
	a := s.activeAgent()
	if a == nil {
		http.Error(w, "no active agent", http.StatusInternalServerError)
		return
	}
	if err := a.SetModel(r.Context(), body.Model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"model": body.Model})
}

func (s *Server) handleEffort(w http.ResponseWriter, _ *http.Request) {
	a := s.activeAgent()
	if a == nil {
		writeJSON(w, map[string]string{"effort": ""})
		return
	}
	current, _ := a.Effort()
	writeJSON(w, map[string]string{"effort": current})
}

func (s *Server) handleSetEffort(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Effort string `json:"effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	a := s.activeAgent()
	if a == nil {
		http.Error(w, "no active agent", http.StatusInternalServerError)
		return
	}
	if err := a.SetEffort(r.Context(), body.Effort); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"effort": body.Effort})
}

// ─── Diagnostics + Capabilities (workspace-scoped) ────────────────

func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	allDiags := s.workspace.Diagnostics(r.Context())

	type diagItem struct {
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Column   int    `json:"column"`
		Severity string `json:"severity"`
		Message  string `json:"message"`
		Source   string `json:"source,omitempty"`
	}

	var result []diagItem
	for filePath, diags := range allDiags {
		relPath := filePath
		if rel, err := filepath.Rel(s.workspace.RootPath, filePath); err == nil {
			relPath = rel
		}
		for _, d := range diags {
			sev := "info"
			switch d.Severity {
			case lsp.DiagnosticSeverityError:
				sev = "error"
			case lsp.DiagnosticSeverityWarning:
				sev = "warning"
			}
			result = append(result, diagItem{
				Path:     relPath,
				Line:     d.Range.Start.Line + 1,
				Column:   d.Range.Start.Character + 1,
				Severity: sev,
				Message:  d.Message,
				Source:   d.Source,
			})
		}
	}
	if result == nil {
		result = []diagItem{}
	}
	sevOrder := map[string]int{"error": 0, "warning": 1, "info": 2}
	slices.SortFunc(result, func(a, b diagItem) int {
		si, sj := sevOrder[a.Severity], sevOrder[b.Severity]
		if si != sj {
			return cmp.Compare(si, sj)
		}
		if a.Path != b.Path {
			return cmp.Compare(a.Path, b.Path)
		}
		return cmp.Compare(a.Line, b.Line)
	})
	writeJSON(w, result)
}

func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	ws := s.workspace
	caps := map[string]any{
		"git":   ws.IsGitRepo(),
		"lsp":   ws.LSP != nil,
		"diffs": ws.Rewind != nil,
	}
	if ws.Rewind == nil {
		caps["notice"] = "This directory is too large for full features. Diffs, checkpoints, and code intelligence are disabled — chat and file browsing still work."
	}
	writeJSON(w, caps)
}

func (s *Server) pollFiles(ctx context.Context) {
	const interval = 2 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	ws := s.workspace
	prevGit := ws.IsGitRepo()
	var prevFingerprint uint64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.wsMu.Lock()
			hasClient := len(s.wsConns) > 0
			s.wsMu.Unlock()
			if !hasClient {
				continue
			}

			gitNow := ws.IsGitRepo()
			if gitNow != prevGit {
				ws.SyncProjectMode()
				s.broadcast(Frame{Type: EvtCapabilitiesChanged})
				if ws.LSP != nil {
					s.broadcast(Frame{Type: EvtDiagnosticsChanged})
				}
				prevGit = gitNow
			}
			// No Rewind = no cheap change signal; the UI re-fetches on user action.
			if ws.Rewind == nil {
				continue
			}
			fp := ws.Rewind.Fingerprint()
			if fp != prevFingerprint {
				s.broadcast(Frame{Type: EvtFilesChanged})
				s.broadcast(Frame{Type: EvtDiffsChanged})
				prevFingerprint = fp
			}
		}
	}
}

func convertMessages(messages []agent.Message) []ConversationMessage {
	var result []ConversationMessage
	for _, m := range messages {
		if m.Hidden {
			continue
		}
		cm := ConversationMessage{Role: string(m.Role)}
		for _, c := range m.Content {
			cc := ConversationContent{}
			if c.Text != "" {
				cc.Text = c.Text
			}
			if c.File != nil && c.File.Data != "" {
				cc.Image = &ConversationImage{Data: c.File.Data, Name: c.File.Name}
			}
			if c.Reasoning != nil && c.Reasoning.Summary != "" {
				cc.Reasoning = &ConversationReasoning{ID: c.Reasoning.ID, Summary: c.Reasoning.Summary}
			}
			if c.ToolCall != nil {
				cc.ToolCall = &ConversationTool{
					ID:   c.ToolCall.ID,
					Name: c.ToolCall.Name,
					Args: c.ToolCall.Args,
					Hint: tool.ExtractHint(c.ToolCall.Args, c.ToolCall.Name),
				}
			}
			if c.ToolResult != nil {
				cc.ToolResult = &ConversationResult{
					ID:      c.ToolResult.ID,
					Name:    c.ToolResult.Name,
					Args:    c.ToolResult.Args,
					Content: c.ToolResult.Content,
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

// constructBackend instantiates an agent by name. "" or BuiltinAgentName
// returns a fresh wingman; otherwise looks up the registration produced by
// availableAgents (auto-detected CLIs + ~/.wingman/agents.json entries)
// and invokes its constructor.
func (s *Server) constructBackend(name string) (code.Agent, error) {
	if name == "" || name == code.BuiltinAgentName {
		w := coder.New(s.workspace, s.config, nil)
		w.SetUI(s)
		// Synchronous: a freshly switched-to agent must have its model set
		// before handleSetAgent broadcasts EvtAgentChanged, or the UI's model
		// refetch races ahead of the pick and the selector renders empty.
		go w.AutoSelectModel(s.ctx)
		return w, nil
	}
	for _, r := range s.availableAgents() {
		if r.Name == name {
			a, err := r.Constructor(s.ctx, s.workspace)
			if err != nil {
				return nil, err
			}
			if us, ok := a.(interface{ SetUI(code.UI) }); ok {
				us.SetUI(s)
			}
			return a, nil
		}
	}
	return nil, fmt.Errorf("unknown agent %q", name)
}
