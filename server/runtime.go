package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agents"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

const instanceHeader = "X-Wingman-Instance"

type WorkspaceScope struct {
	WorkspaceID string `json:"workspaceId"`
	InstanceID  string `json:"instanceId"`
}

type AgentEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func supportsDelete(a code.Agent) bool {
	if d, ok := a.(interface{ SupportsDelete() bool }); ok {
		return d.SupportsDelete()
	}
	return true
}

type SessionRef struct {
	WorkspaceID string `json:"workspaceId"`
	BackendID   string `json:"backendId"`
	SessionID   string `json:"sessionId"`
}

func workspaceScope(root string) WorkspaceScope {
	// Connection identity is for the opened root, independent of legacy disk layout.
	if resolved, err := pathutil.Resolve(root); err == nil {
		root = resolved
	}
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return WorkspaceScope{WorkspaceID: hex.EncodeToString(sum[:]), InstanceID: uuid.NewString()}
}

func (s *Server) InstanceID() string { return s.scope.InstanceID }

func (s *Server) checkInstance(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/v2/bootstrap" {
			instance := r.Header.Get(instanceHeader)
			if instance == "" {
				instance = r.URL.Query().Get("instance")
			} // sockets and download links
			if instance != s.scope.InstanceID || s.ctx.Err() != nil {
				http.Error(w, "workspace instance changed; reload the workspace", http.StatusConflict)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Each backend owns its agent, turn manager, and session controllers for the
// workspace lifetime. Browser navigation never replaces execution ownership.
type backendRuntime struct {
	*Server
	id             string
	agent          code.Agent
	turns          *code.TurnManager
	mu             sync.Mutex
	sessions       map[string]*sessionController
	createMu       sync.Mutex
	creations      map[string]creationReceipt
	taskPumpMu     sync.Mutex
	taskPumps      map[*task.Registry]bool
	operationMu    sync.Mutex
	operations     sync.WaitGroup
	closing        bool
	modelDiscovery singleflight.Group
}

// Operations may nest (commands refresh metadata), but admission closes before
// shutdown waits. Backend resources outlive every admitted operation.
func (b *backendRuntime) beginOperation() bool {
	b.operationMu.Lock()
	defer b.operationMu.Unlock()
	if b.closing || b.ctx.Err() != nil {
		return false
	}
	b.operations.Add(1)
	return true
}

func (s *Server) backendOperation(fn func(*backendRuntime, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.backendHandler(func(b *backendRuntime, w http.ResponseWriter, r *http.Request) {
		if !b.beginOperation() {
			http.Error(w, "workspace closed", 409)
			return
		}
		defer b.operations.Done()
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(b.ctx, cancel)
		defer func() { stop(); cancel() }()
		fn(b, w, r.WithContext(ctx))
	})
}

func (s *Server) bindBackend(id string, a code.Agent) *backendRuntime {
	b := &backendRuntime{Server: s, id: id, agent: a, sessions: map[string]*sessionController{}, creations: map[string]creationReceipt{}}
	if ui, ok := a.(interface{ SetUI(code.UI) }); ok {
		ui.SetUI(b)
	}
	b.turns = code.NewTurnManager(tool.WithProgressSink(s.ctx, b.onToolProgress), a, b.handleTurnEvent)
	if source, ok := a.(code.SessionUpdateSource); ok {
		source.SetSessionUpdateHandler(func(sid string) {
			if sid == "" {
				return
			}
			b.session(sid).refreshSettings()
			b.broadcast(Frame{Type: EvtSessionsChanged})
			b.broadcast(Frame{Type: EvtSkillsChanged})
		})
	}
	return b
}

// backendEntry holds either a running backend or its shared startup. Startup
// publishes runtime and err by closing done; failures are removed for retry.
type backendEntry struct {
	done    chan struct{}
	runtime *backendRuntime
	err     error
}

func (s *Server) backend(id string) (*backendRuntime, error) {
	return s.resolveBackend(id, func(ctx context.Context) (code.Agent, error) {
		return agents.NewExternal(ctx, s.workspace, id)
	})
}

func (s *Server) resolveBackend(id string, create func(context.Context) (code.Agent, error)) (*backendRuntime, error) {
	s.backendsMu.Lock()
	if err := s.ctx.Err(); err != nil {
		s.backendsMu.Unlock()
		return nil, err
	}
	if id == "" {
		s.backendsMu.Unlock()
		return nil, errors.New("backend id required")
	}
	if entry := s.backends[id]; entry != nil {
		runtime := entry.runtime
		s.backendsMu.Unlock()
		if runtime != nil {
			return runtime, nil
		}
		select {
		case <-entry.done:
			return entry.runtime, entry.err
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		}
	}
	entry := &backendEntry{done: make(chan struct{})}
	s.backends[id] = entry
	// Reserve before releasing the registry lock so Close joins late startup.
	s.background.Add(1)
	s.backendsMu.Unlock()
	defer s.background.Done()
	a, err := create(s.ctx)
	s.backendsMu.Lock()
	if err == nil {
		err = s.ctx.Err()
	}
	if err == nil {
		entry.runtime = s.bindBackend(id, a)
	} else {
		delete(s.backends, id)
	}
	entry.err = err
	close(entry.done)
	s.backendsMu.Unlock()
	if err != nil && a != nil {
		_ = a.Close()
	}
	return entry.runtime, entry.err
}

func (b *backendRuntime) session(id string) *sessionController {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.sessions[id]; c != nil {
		return c
	}
	c := newSessionController(b, id)
	b.sessions[id] = c
	return c
}

func (s *Server) backendHandler(fn func(*backendRuntime, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := s.backend(r.PathValue("backend"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fn(b, w, r)
	}
}

func (s *Server) registerSessionRoutes(r chi.Router) {
	r.Route("/v2", func(r chi.Router) {
		r.Get("/bootstrap", func(w http.ResponseWriter, r *http.Request) {
			available, err := agents.Available()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			backends := []AgentEntry{{ID: code.BuiltinAgentName, Name: "Wingman"}}
			for _, a := range available {
				backends = append(backends, AgentEntry{ID: a.ID, Name: a.Name})
			}
			writeJSON(w, struct {
				WorkspaceScope
				Protocol int          `json:"protocol"`
				Backends []AgentEntry `json:"backends"`
			}{s.scope, 2, backends})
		})
		r.HandleFunc("/events", s.handleWebSocket)
		r.Route("/backends/{backend}", func(r chi.Router) {
			r.Get("/settings", s.backendOperation((*backendRuntime).handleDefaultSettings))
			r.Post("/models/refresh", s.backendOperation((*backendRuntime).handleRefreshModels))
			r.Get("/sessions", s.backendOperation((*backendRuntime).handleSessions))
			r.Post("/sessions", s.backendHandler((*backendRuntime).handleNewSession))
			r.Route("/sessions/{id}", func(r chi.Router) {
				r.Get("/reviews", s.backendOperation((*backendRuntime).handleTurnReviews))
				r.Get("/reviews/{turnID}", s.backendOperation((*backendRuntime).handleTurnReviews))
				r.Post("/reviews/{turnID}/undo", s.backendOperation((*backendRuntime).handleTurnUndo))
				r.Post("/commands", s.backendHandler((*backendRuntime).handleCommand))
				r.Get("/tasks", s.backendOperation((*backendRuntime).handleTasks))
				r.Get("/tasks/{taskID}", s.backendOperation((*backendRuntime).handleTask))
				r.Post("/tasks/{taskID}/stop", s.backendOperation((*backendRuntime).handleTaskStop))
				r.Get("/schedules", s.backendOperation((*backendRuntime).handleSchedules))
				r.Delete("/schedules/{scheduleID}", s.backendOperation((*backendRuntime).handleScheduleDelete))
				r.Post("/schedules/{scheduleID}/pause", s.backendOperation((*backendRuntime).handleSchedulePause))
				r.Post("/schedules/{scheduleID}/resume", s.backendOperation((*backendRuntime).handleScheduleResume))
			})
		})
	})
}

func (b *backendRuntime) handleSessions(w http.ResponseWriter, r *http.Request) {
	infos, err := b.agent.ListSessions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	out := make([]SessionEntry, 0, len(infos))
	for _, si := range infos {
		ent := SessionEntry{ID: si.ID, Title: si.Title}
		if !si.UpdatedAt.IsZero() {
			ent.UpdatedAt = si.UpdatedAt.Format(time.RFC3339)
		}
		out = append(out, ent)
	}
	slices.SortFunc(out, func(a, b SessionEntry) int { return strings.Compare(b.UpdatedAt, a.UpdatedAt) })
	writeJSON(w, out)
}

type Command struct {
	ID         string               `json:"id"`
	Epoch      string               `json:"epoch,omitempty"`
	Type       string               `json:"type"`
	InputID    string               `json:"inputId,omitempty"`
	Text       string               `json:"text,omitempty"`
	Files      []string             `json:"files,omitempty"`
	Images     []string             `json:"images,omitempty"`
	Intent     code.TurnInputIntent `json:"intent,omitempty"`
	ClearQueue bool                 `json:"clearQueue,omitempty"`
	PromptID   string               `json:"promptId,omitempty"`
	Action     string               `json:"action,omitempty"`
	Scope      string               `json:"scope,omitempty"`
	Content    map[string]any       `json:"content,omitempty"`
	Model      *string              `json:"model,omitempty"`
	Effort     *string              `json:"effort,omitempty"`
	Mode       *string              `json:"mode,omitempty"`
	Unattended *bool                `json:"unattended,omitempty"`
	Source     string               `json:"source,omitempty"`
	Title      string               `json:"title,omitempty"`
}

type Receipt struct {
	ID      string     `json:"id"`
	Ref     SessionRef `json:"ref"`
	Epoch   string     `json:"epoch"`
	Outcome string     `json:"outcome"`
}

type commandReceipt struct {
	fingerprint [32]byte
	receipt     Receipt
	status      int
	message     string
	done        chan struct{}
}

type creationReceipt struct {
	fingerprint [32]byte
	receipt     Receipt
}

func commandFingerprint(command Command) [32]byte {
	data, _ := json.Marshal(command)
	return sha256.Sum256(data)
}

func (b *backendRuntime) handleNewSession(w http.ResponseWriter, r *http.Request) {
	var command Command
	if err := decodeJSONRequest(w, r, &command, 32<<20); err != nil {
		return
	}
	if !b.beginOperation() {
		http.Error(w, "workspace closed", 409)
		return
	}
	defer b.operations.Done()
	if command.ID == "" || command.Type != "create" && command.Type != "fork" {
		http.Error(w, "create or fork command and request id required", 400)
		return
	}
	forker, canFork := b.agent.(code.SessionForker)
	if command.Type == "fork" && (!canFork || command.Source == "") {
		http.Error(w, "fork requires a source session on an agent that supports forking", 400)
		return
	}
	b.createMu.Lock()
	defer b.createMu.Unlock()
	fingerprint := commandFingerprint(command)
	if previous, ok := b.creations[command.ID]; ok {
		if previous.fingerprint != fingerprint {
			http.Error(w, "request id reused with different content", 409)
			return
		}
		writeJSON(w, previous.receipt)
		return
	}
	var id string
	var err error
	if command.Type == "fork" {
		id, err = forker.ForkSession(code.WithSessionID(b.ctx, command.Source), command.Source, command.Title)
	} else {
		id, err = b.agent.NewSession(b.ctx)
	}
	if err != nil {
		status := 502
		if errors.Is(err, code.ErrTurnInProgress) {
			status = 409
		}
		http.Error(w, err.Error(), status)
		return
	}
	c := b.session(id)
	c.opMu.Lock()
	// Record creation before configuring it: retries must never create a second session.
	receipt := Receipt{ID: command.ID, Ref: c.ref, Epoch: c.epoch, Outcome: "created"}
	b.creations[command.ID] = creationReceipt{fingerprint, receipt}
	c.loaded = true
	c.replaceHistory()
	c.mu.Lock()
	c.state.Status = "ready"
	c.publishStateLocked()
	c.mu.Unlock()
	c.opMu.Unlock()
	c.refreshSettings()
	b.ensureTaskPump(id)
	b.broadcast(Frame{Type: EvtSessionsChanged})
	writeJSON(w, receipt)
}

func (b *backendRuntime) handleCommand(w http.ResponseWriter, r *http.Request) {
	var command Command
	if err := decodeJSONRequest(w, r, &command, 32<<20); err != nil {
		return
	}
	if !b.beginOperation() {
		http.Error(w, "workspace closed", 409)
		return
	}
	defer b.operations.Done()
	if command.ID == "" || command.Epoch == "" {
		http.Error(w, "request id and session epoch required", 400)
		return
	}
	c := b.session(r.PathValue("id"))
	record := c.command(r.Context(), command)
	if record.status != 0 {
		http.Error(w, record.message, record.status)
	} else {
		writeJSON(w, record.receipt)
	}
}

// All commands share one receipt ledger, including prompts. Reserve IDs before
// waiting on opMu so an in-flight operation cannot have its ID reused to answer
// its own prompt. Never hold a receipt/projection lock while doing backend I/O.
func (c *sessionController) command(ctx context.Context, command Command) commandReceipt {
	failure := func(message string) commandReceipt { return commandReceipt{status: 409, message: message} }
	if c.epoch != command.Epoch {
		return failure("session epoch changed; subscribe again")
	}
	fingerprint := commandFingerprint(command)
	c.receiptMu.Lock()
	if previous := c.receipts[command.ID]; previous != nil {
		c.receiptMu.Unlock()
		if previous.fingerprint != fingerprint {
			return failure("request id reused with different content")
		}
		select {
		case <-previous.done:
			return *previous
		case <-ctx.Done():
			return failure(ctx.Err().Error())
		case <-c.backend.ctx.Done():
			return failure("workspace closed")
		}
	}
	record := &commandReceipt{fingerprint: fingerprint, done: make(chan struct{}), receipt: Receipt{ID: command.ID, Ref: c.ref, Epoch: c.epoch, Outcome: "accepted"}}
	c.receipts[command.ID] = record
	c.receiptMu.Unlock()
	defer close(record.done)
	var err error
	if command.Type == "prompt_response" {
		err = c.resolvePrompt(command)
	} else {
		c.opMu.Lock()
		defer c.opMu.Unlock()
		err = c.loadLocked()
		if err == nil {
			err = c.execute(command)
		}
	}
	if err != nil {
		record.status, record.message = http.StatusConflict, err.Error()
	}
	return *record
}

func (c *sessionController) execute(command Command) error {
	b, sid := c.backend, c.ref.SessionID
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if c.deleted {
		return errors.New("session deleted")
	}
	switch command.Type {
	case "send", "queue_update":
		if command.Type == "send" && command.InputID != "" && command.InputID != command.ID {
			return errors.New("input id must equal the send request id")
		}
		id := command.InputID
		if id == "" {
			id = command.ID
		}
		input := code.TurnInput{ID: id, Intent: command.Intent, Origin: "user", Display: &code.TurnInputDisplay{Text: command.Text, Files: command.Files, Images: command.Images}}
		input.Content = b.buildInput(command)
		if command.Type == "queue_update" {
			if err := b.turns.ReplaceQueued(sid, id, input); err != nil {
				return err
			}
		} else {
			if _, err := b.turns.Submit(b.ctx, sid, input); err != nil {
				return err
			}
		}
		b.ensureTaskPump(sid)
	case "compact":
		c.mu.Lock()
		c.state.Error, c.state.Compaction = nil, nil
		c.mu.Unlock()
		if err := b.turns.StartCompact(sid, command.Text, func(result harness.CompactResult, err error) {
			b.finishCompaction(sid, result, err)
		}); err != nil {
			return err
		}
	case "queue_remove":
		if err := b.turns.RemoveQueued(sid, command.InputID); err != nil {
			return err
		}
	case "cancel":
		if command.ClearQueue {
			if err := b.turns.CancelAll(sid); err != nil {
				return err
			}
		} else {
			if err := b.turns.CancelCurrent(sid); err != nil {
				return err
			}
		}
	case "queue_clear":
		if err := b.turns.ClearQueue(sid); err != nil {
			return err
		}
	case "queue_resume":
		b.turns.Resume(sid)
		if err := b.turns.Snapshot(sid).Error; err != nil {
			return err
		}
	case "settings":
		ctx := code.WithSessionID(b.ctx, sid)
		// Always refresh, including partial failures or provider-normalized values.
		defer c.refreshSettings()
		if command.Model != nil {
			if err := b.agent.SetModel(ctx, sid, *command.Model); err != nil {
				return err
			}
		}
		if command.Effort != nil {
			if err := b.agent.SetEffort(ctx, sid, *command.Effort); err != nil {
				return err
			}
		}
		if command.Mode != nil {
			if err := b.agent.SetMode(ctx, sid, *command.Mode); err != nil {
				return err
			}
		}
		if command.Unattended != nil {
			policy, ok := b.agent.(code.UnattendedAgent)
			if !ok {
				return errors.ErrUnsupported
			}
			if err := policy.SetUnattended(ctx, sid, *command.Unattended); err != nil {
				return err
			}
		}
	case "delete":
		if !supportsDelete(b.agent) {
			return errors.ErrUnsupported
		}
		// A terminal tombstone is retained for this server lifetime. Late callbacks
		// cannot recreate a deleted view or admit a new input under the old identity.
		if err := b.turns.StopSession(b.ctx, sid); err != nil {
			return err
		}
		if err := b.agent.DeleteSession(b.ctx, sid); err != nil {
			return err
		}
		c.deleted = true
		c.mu.Lock()
		for _, prompt := range c.prompts {
			prompt.reply <- Command{Action: "cancel"}
		}
		c.prompts = map[string]pendingPrompt{}
		c.state.Status = "deleted"
		c.state.Phase = "idle"
		c.state.PendingInputs = []TurnQueueEntry{}
		c.state.Prompts = []PromptView{}
		c.publishStateLocked()
		c.mu.Unlock()
		b.broadcast(Frame{Type: EvtSessionsChanged})
	default:
		return fmt.Errorf("unknown session command %q", command.Type)
	}
	b.sendTurnSnapshot(sid)
	return nil
}

func (b *backendRuntime) handleDefaultSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, b.settings(""))
}

func (b *backendRuntime) onToolProgress(ctx context.Context, callID, text string) {
	if sid := code.SessionIDFromContext(ctx); sid != "" {
		b.sendSession(sid, Frame{Type: EvtToolProgress, ID: callID, Text: text})
	}
}
