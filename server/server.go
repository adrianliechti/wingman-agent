package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	"github.com/adrianliechti/wingman-agent/pkg/system"
)

//go:embed static/*
var staticFiles embed.FS

var StaticFS, _ = fs.Sub(staticFiles, "static")

type Server struct {
	port int

	// workspace owns the workdir-shared resources (Root, MCP, LSP, Rewind,
	// Skills). Each session is a code.Agent bound to it.
	workspace *code.Workspace

	// config carries the upstream API client + late-bound model/effort
	// getters. Workspace.NewAgent Derives each session's own Config from
	// this. Server.handleModels uses it directly to list available models
	// without needing a session.
	config *agent.Config

	// ctx lives for the lifetime of the server. Agent turns and background
	// goroutines (pollFiles, async MCP warmup) tie their cancellation to
	// this — NOT to any HTTP request ctx. Tying a Send to r.Context() would
	// cancel the agent mid-turn on a WS disconnect/reconnect.
	ctx context.Context

	mux *http.ServeMux

	sessionsDir string

	// Sessions held in memory. sessionsOrder keeps iteration deterministic
	// across reconnects so the UI's auto-pick of "first session" is stable.
	mu            sync.Mutex
	sessions      map[string]*Session
	sessionsOrder []string

	// Shared model/effort selection. Sessions read these via late-bound
	// Config.Model/Effort, so changing the picker doesn't need to iterate.
	cfgMu  sync.Mutex
	model  string
	effort string

	// WebSocket fan-out. Every event goes to every connected client tagged
	// with its session id (or untagged for server-level events); the React
	// side dispatches.
	wsMu    sync.Mutex
	wsConns map[*websocket.Conn]struct{}
}

func New(ctx context.Context, workDir string, port int) (*Server, error) {
	cfg, err := agent.DefaultConfig()
	if err != nil {
		return nil, err
	}
	ws, err := code.NewWorkspace(workDir)
	if err != nil {
		return nil, err
	}

	s := &Server{
		port:        port,
		workspace:   ws,
		config:      cfg,
		ctx:         ctx,
		sessionsDir: filepath.Join(filepath.Dir(ws.MemoryPath), "sessions"),
		sessions:    map[string]*Session{},
		wsConns:     map[*websocket.Conn]struct{}{},
	}

	// Synchronous workspace probe so /api/capabilities is authoritative by
	// the time the browser first hits it. MCP setup runs async — slow MCP
	// servers shouldn't block startup.
	ws.WarmUp()
	go func() {
		if err := ws.InitMCP(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "MCP init warning: %v\n", err)
		}
	}()

	s.autoSelectModel(ctx)

	// Poll for outside-the-agent file changes so the FileTree/Diffs panels
	// reflect terminal `rm`s and IDE saves. Polling beats fsnotify here:
	// zero FDs (kqueue's per-dir watcher cost was the original $HOME crash),
	// one path everywhere, ≤2s latency — fine for this UI.
	go s.pollFiles(ctx)

	s.mux = http.NewServeMux()
	s.registerRoutes(s.mux)

	return s, nil
}

// Close releases the workspace (Root, MCP, LSP, Rewind, scratch). Sessions
// share these resources, so individual session Close is now a no-op.
func (s *Server) Close() {
	if s.workspace != nil {
		s.workspace.Close()
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleWebSocketURL(w http.ResponseWriter, r *http.Request) {
	proto := "ws"
	if r.TLS != nil {
		proto = "wss"
	}
	writeJSON(w, map[string]string{"url": fmt.Sprintf("%s://%s/ws", proto, r.Host)})
}

func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	port, err := system.FreePort(s.port)
	if err != nil {
		return err
	}
	s.port = port

	server := &http.Server{
		Addr:    fmt.Sprintf("localhost:%d", s.port),
		Handler: s,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		cancel()
		server.Close()
	}()

	url := fmt.Sprintf("http://localhost:%d", s.port)
	fmt.Fprintf(os.Stderr, "Wingman running at %s\n", url)

	if os.Getenv("WINGMAN_NO_BROWSER") == "" {
		openBrowser(url)
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
	mux.HandleFunc("GET /api/diffs", s.handleDiffs)
	mux.HandleFunc("POST /api/diffs/revert", s.handleDiffRevert)
	mux.HandleFunc("GET /api/checkpoints", s.handleCheckpoints)
	mux.HandleFunc("POST /api/checkpoints/{hash}/restore", s.handleCheckpointRestore)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("POST /api/sessions/new", s.handleNewSession)
	mux.HandleFunc("POST /api/sessions/{id}/load", s.handleLoadSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /api/model", s.handleModel)
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("POST /api/model", s.handleSetModel)
	mux.HandleFunc("GET /api/effort", s.handleEffort)
	mux.HandleFunc("POST /api/effort", s.handleSetEffort)
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

func (s *Server) registerSession(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
	s.sessionsOrder = append(s.sessionsOrder, sess.ID)
}

func (s *Server) getSession(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// allSessions returns a snapshot of every in-memory session in creation
// order. Callers can iterate without holding s.mu.
func (s *Server) allSessions() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, id := range s.sessionsOrder {
		if sess, ok := s.sessions[id]; ok {
			out = append(out, sess)
		}
	}
	return out
}

// sessionFromRequest resolves the target session for a per-session HTTP
// endpoint via ?session=…. Returns nil if missing or unknown — callers
// 404 / 503 their response accordingly. (Per-session routes that take {id}
// in the path — load/delete — read it themselves.)
func (s *Server) sessionFromRequest(r *http.Request) *Session {
	return s.getSession(r.URL.Query().Get("session"))
}

// broadcast emits a workspace-level event (no session id) to every connected
// WebSocket. Use this for state that applies to every session view: file
// tree changes, capabilities, the sessions list.
func (s *Server) broadcast(f Frame) {
	f.Session = ""
	s.send(f)
}

// send marshals a Frame and fans it out to every WS client. Call sites
// build Frames inline — `s.broadcast(...)` for workspace events,
// `sess.send(...)` for per-session events (it sets f.Session).
func (s *Server) send(f Frame) {
	data, err := json.Marshal(f)
	if err != nil {
		return
	}

	s.wsMu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.wsConns))
	for c := range s.wsConns {
		conns = append(conns, c)
	}
	s.wsMu.Unlock()

	for _, c := range conns {
		c.Write(context.Background(), websocket.MessageText, data)
	}
}

func (s *Server) currentModel() string {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.model
}

func (s *Server) currentEffort() string {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.effort
}

func (s *Server) setModel(model string) {
	s.cfgMu.Lock()
	s.model = model
	s.cfgMu.Unlock()
}

func (s *Server) setEffort(effort string) {
	s.cfgMu.Lock()
	s.effort = effort
	s.cfgMu.Unlock()
}

// autoSelectModel picks a default model on startup if none is set. Prefers
// the curated list (code.AvailableModels) over whatever the API happens to
// return first.
func (s *Server) autoSelectModel(ctx context.Context) {
	if s.currentModel() != "" {
		return
	}
	models, err := s.config.Models(ctx)
	if err != nil || len(models) == 0 {
		return
	}

	upstream := make(map[string]bool, len(models))
	for _, m := range models {
		upstream[m.ID] = true
	}
	for _, m := range code.AvailableModels {
		if upstream[m.ID] {
			s.setModel(m.ID)
			return
		}
	}
	s.setModel(models[0].ID)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	// Combine on-disk (saved) and in-memory (possibly empty/new) sessions.
	// Saved entries carry the real CreatedAt/Title; in-memory-only entries
	// are brand new with nothing on disk yet, so they get "now". Timestamps
	// are RFC3339 so the React side can parse them with `new Date(...)`
	// across browsers — the previous "YYYY-MM-DD HH:MM" format isn't
	// standard for Date parsing and gave NaN on some runtimes.
	seen := map[string]bool{}
	result := []SessionEntry{}

	saved, _ := session.List(s.sessionsDir)
	for _, sess := range saved {
		seen[sess.ID] = true
		result = append(result, SessionEntry{
			ID:        sess.ID,
			Title:     sess.Title,
			CreatedAt: sess.CreatedAt.Format(time.RFC3339),
			UpdatedAt: sess.UpdatedAt.Format(time.RFC3339),
		})
	}

	nowStr := time.Now().Format(time.RFC3339)
	for _, sess := range s.allSessions() {
		if seen[sess.ID] {
			continue
		}
		result = append(result, SessionEntry{
			ID:        sess.ID,
			CreatedAt: nowStr,
			UpdatedAt: nowStr,
		})
	}

	// RFC3339 sorts chronologically as a string, so no parallel time.Time
	// field needed for ordering.
	sort.Slice(result, func(i, j int) bool {
		return result[i].UpdatedAt > result[j].UpdatedAt
	})

	writeJSON(w, result)
}

func (s *Server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	sess := s.newSession(newSessionID())
	s.registerSession(sess)
	// Brand-new sessions are empty by definition. No session_state push —
	// it would race the lazy-create-on-send flow (client appends user entry
	// after the POST returns, server's empty-snapshot would erase it). The
	// creating client creates its slot locally on receiving the {id}; other
	// clients get one when they click the sidebar entry.
	s.broadcast(Frame{Type: EvtSessionsChanged})

	writeJSON(w, map[string]string{"id": sess.ID})
}

func (s *Server) handleLoadSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}

	sess := s.getSession(id)
	if sess == nil {
		// Not in memory — load from disk into a new in-memory session so
		// further sends in this session resume correctly.
		saved, err := session.Load(s.sessionsDir, id)
		if err != nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		newSess := s.newSession(id)
		newSess.Agent.Messages = saved.State.Messages
		newSess.Agent.Usage = saved.State.Usage
		s.registerSession(newSess)
		sess = newSess
		s.broadcast(Frame{Type: EvtSessionsChanged})
	}

	// Push full state via WS so every tab observing this session sees the
	// same snapshot the requesting tab will. The HTTP response is just an
	// ack — the client doesn't need the payload here.
	sess.sendState()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	sess, inMem := s.sessions[id]
	if inMem {
		delete(s.sessions, id)
		for i, sid := range s.sessionsOrder {
			if sid == id {
				s.sessionsOrder = append(s.sessionsOrder[:i], s.sessionsOrder[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()

	if inMem {
		sess.close()
	}

	if err := session.Delete(s.sessionsDir, id); err != nil && !os.IsNotExist(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcast(Frame{Type: EvtSessionsChanged})

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"model": s.currentModel()})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.config.Models(r.Context())
	if err != nil {
		writeJSON(w, []map[string]string{})
		return
	}

	upstream := make(map[string]bool, len(models))
	for _, m := range models {
		upstream[m.ID] = true
	}

	result := make([]map[string]string, 0, len(code.AvailableModels))
	for _, m := range code.AvailableModels {
		if !upstream[m.ID] {
			continue
		}
		result = append(result, map[string]string{
			"id":   m.ID,
			"name": m.Name,
		})
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

	s.setModel(body.Model)

	writeJSON(w, map[string]string{"model": body.Model})
}

func (s *Server) handleEffort(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"effort": s.currentEffort()})
}

func (s *Server) handleSetEffort(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Effort string `json:"effort"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	switch body.Effort {
	case "", "auto":
		body.Effort = ""
	case "low", "medium", "high":
	default:
		http.Error(w, "effort must be auto, low, medium, or high", http.StatusBadRequest)
		return
	}

	s.setEffort(body.Effort)
	writeJSON(w, map[string]string{"effort": body.Effort})
}

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
	sort.Slice(result, func(i, j int) bool {
		si, sj := sevOrder[result[i].Severity], sevOrder[result[j].Severity]
		if si != sj {
			return si < sj
		}
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		return result[i].Line < result[j].Line
	})

	writeJSON(w, result)
}

// pollFiles watches the working dir for external changes. Same shape as the
// single-session version, but the workspace probes target any session's
// agent (they all watch the same dir).
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
				// Workspace rebuilds LSP once for the whole server when git
				// status flips (typically: agent ran `git init` in a scratch
				// dir). All sessions inherit via Workspace.LSP.
				ws.SyncProjectMode()
				s.broadcast(Frame{Type: EvtCapabilitiesChanged})
				if ws.LSP != nil {
					s.broadcast(Frame{Type: EvtDiagnosticsChanged})
				}
				prevGit = gitNow
			}

			if ws.Rewind == nil {
				s.broadcast(Frame{Type: EvtFilesChanged})
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

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
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

func convertMessages(messages []agent.Message) []ConversationMessage {
	var result []ConversationMessage

	for _, m := range messages {
		if m.Hidden {
			continue
		}

		cm := ConversationMessage{
			Role: string(m.Role),
		}

		for _, c := range m.Content {
			cc := ConversationContent{}

			if c.Text != "" {
				cc.Text = c.Text
			}

			if c.Reasoning != nil && c.Reasoning.Summary != "" {
				cc.Reasoning = &ConversationReasoning{
					ID:      c.Reasoning.ID,
					Summary: c.Reasoning.Summary,
				}
			}

			if c.ToolCall != nil {
				cc.ToolCall = &ConversationTool{
					ID:   c.ToolCall.ID,
					Name: c.ToolCall.Name,
					Args: c.ToolCall.Args,
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

