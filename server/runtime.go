package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agents"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
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
	sum := sha256.Sum256([]byte(canonicalPath(root)))
	return WorkspaceScope{WorkspaceID: hex.EncodeToString(sum[:]), InstanceID: uuid.NewString()}
}

func (s *Server) InstanceID() string { return s.scope.InstanceID }

func (s *Server) checkInstance(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		instance := r.Header.Get(instanceHeader)
		if instance == "" {
			instance = r.URL.Query().Get("instance")
		}
		// Discovery is initially unscoped. Later refreshes carry the instance
		// and must reject a replacement workspace like all other operations.
		protected := (strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/workspace") || r.URL.Path == "/lsp" || r.URL.Path == "/dap" || r.URL.Path == "/content" || r.URL.Path == "/preview"
		if protected || r.URL.Path == "/api/workspace" && instance != "" {
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
	b := &backendRuntime{Server: s, id: id, agent: a, sessions: map[string]*sessionController{}}
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

// discard forgets a controller that never loaded, such as one for an
// unknown session id, so lookups cannot grow the session map.
func (b *backendRuntime) discard(id string, c *sessionController) {
	c.opMu.Lock()
	loaded := c.loaded
	c.opMu.Unlock()
	c.mu.Lock()
	attached := c.ahp != nil
	c.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !loaded && !attached && b.sessions[id] == c {
		delete(b.sessions, id)
	}
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
		r.Post("/backends/{backend}/models/refresh", s.backendOperation((*backendRuntime).handleRefreshModels))
		r.Route("/chats/{chat}", func(r chi.Router) {
			r.Use(s.chatRoute)
			r.Get("/reviews", s.backendOperation((*backendRuntime).handleTurnReviews))
			r.Get("/reviews/{turnID}", s.backendOperation((*backendRuntime).handleTurnReviews))
			r.Post("/reviews/{turnID}/undo", s.backendOperation((*backendRuntime).handleTurnUndo))
			r.Get("/tasks", s.backendOperation((*backendRuntime).handleTasks))
			r.Get("/tasks/{taskID}", s.backendOperation((*backendRuntime).handleTask))
			r.Post("/tasks/{taskID}/stop", s.backendOperation((*backendRuntime).handleTaskStop))
			r.Get("/schedules", s.backendOperation((*backendRuntime).handleSchedules))
			r.Delete("/schedules/{scheduleID}", s.backendOperation((*backendRuntime).handleScheduleDelete))
			r.Post("/schedules/{scheduleID}/pause", s.backendOperation((*backendRuntime).handleSchedulePause))
			r.Post("/schedules/{scheduleID}/resume", s.backendOperation((*backendRuntime).handleScheduleResume))
		})
	})
}

// chatRoute resolves the AHP chat URI in the path to the native session the
// handlers address by backend and id.
func (s *Server) chatRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chat, err := url.PathUnescape(chi.URLParam(r, "chat"))
		if err != nil {
			http.Error(w, "invalid chat URI", http.StatusBadRequest)
			return
		}
		_, ch, err := s.ahpProject().ensureSession(chat)
		if err != nil || ch == nil {
			http.Error(w, "chat not found", http.StatusNotFound)
			return
		}
		r.SetPathValue("backend", ch.controller.backend.id)
		r.SetPathValue("id", ch.controller.ref.SessionID)
		next.ServeHTTP(w, r)
	})
}

type Command struct {
	requireIdle bool
	wait        bool                         // steering that waits for a turn instead of starting one
	sync        bool                         // settings that its AHP chat wants when they run
	option      *t.ConfirmationOption        // the approval option an AHP client chose
	answers     map[string]t.ChatInputAnswer // an AHP client's answers with its drafts, as its reducer keeps them

	ID         string               `json:"id"`
	Epoch      string               `json:"epoch,omitempty"`
	Type       string               `json:"type"`
	InputID    string               `json:"inputId,omitempty"`
	Text       string               `json:"text,omitempty"`
	Context    []string             `json:"context,omitempty"` // model-only attachment text, read before the input
	Message    json.RawMessage      `json:"message,omitempty"` // the input in its client's own form
	Files      []string             `json:"files,omitempty"`
	Images     []string             `json:"images,omitempty"`
	Intent     code.TurnInputIntent `json:"intent,omitempty"`
	ClearQueue bool                 `json:"clearQueue,omitempty"`
	Order      []string             `json:"order,omitempty"`
	PromptID   string               `json:"promptId,omitempty"`
	Action     string               `json:"action,omitempty"`
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

func commandFingerprint(command Command) [32]byte {
	data, _ := json.Marshal(command)
	return sha256.Sum256(data)
}

// createSession creates a native session, or forks one. Callers own retries:
// AHP creation is serialised per URI and recorded in the session's alias.
func (b *backendRuntime) createSession(command Command) commandReceipt {
	failure := func(status int, message string) commandReceipt {
		return commandReceipt{status: status, message: message}
	}
	if !b.beginOperation() {
		return failure(409, "workspace closed")
	}
	defer b.operations.Done()
	if command.Type != "create" && command.Type != "fork" {
		return failure(400, "create or fork command required")
	}
	forker, canFork := b.agent.(code.SessionForker)
	if command.Type == "fork" && (!canFork || command.Source == "") {
		return failure(400, "fork requires a source session on an agent that supports forking")
	}
	var id string
	var err error
	if command.Type == "fork" && command.InputID != "" {
		through, ok := b.agent.(code.SessionTurnForker)
		if !ok {
			return failure(400, "agent cannot fork through an earlier turn")
		}
		id, err = through.ForkSessionThrough(code.WithSessionID(b.ctx, command.Source), command.Source, command.InputID, command.Title)
	} else if command.Type == "fork" {
		id, err = forker.ForkSession(code.WithSessionID(b.ctx, command.Source), command.Source, command.Title)
	} else {
		id, err = b.agent.NewSession(b.ctx)
	}
	if err != nil {
		status := 502
		if errors.Is(err, code.ErrTurnInProgress) {
			status = 409
		}
		return failure(status, err.Error())
	}
	c := b.session(id)
	c.opMu.Lock()
	receipt := Receipt{ID: command.ID, Ref: c.ref, Epoch: c.epoch, Outcome: "created"}
	c.loaded = true
	c.replaceHistory()
	c.mu.Lock()
	c.state.Status = "ready"
	c.publishStateLocked()
	c.mu.Unlock()
	c.opMu.Unlock()
	c.refreshSettings()
	b.ensureTaskPump(id)
	return commandReceipt{receipt: receipt}
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
	c.receiptOrder = append(c.receiptOrder, command.ID)
	c.pruneReceiptsLocked()
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

const receiptLimit = 1024

// pruneReceiptsLocked forgets the oldest completed receipts beyond the limit;
// in-flight ones stay so their IDs cannot be reused. Caller holds receiptMu.
func (c *sessionController) pruneReceiptsLocked() {
	for i := 0; len(c.receipts) > receiptLimit && i < len(c.receiptOrder); {
		id := c.receiptOrder[i]
		select {
		case <-c.receipts[id].done:
			delete(c.receipts, id)
			c.receiptOrder = slices.Delete(c.receiptOrder, i, i+1)
		default:
			i++
		}
	}
}

// cuttable reports whether the agent's history holds an input, after whose turn
// it can be cut. A compaction is no input of the history.
func (c *sessionController) cuttable(inputID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, turn := range c.hostTurns {
		if turn.compact && turn.entries[0].InputID == inputID {
			return false
		}
	}
	return slices.ContainsFunc(c.entries, func(e TranscriptEntry) bool { return e.Type == "user" && e.InputID == inputID })
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
		input := code.TurnInput{ID: id, Intent: command.Intent, Wait: command.wait, Origin: "user", Context: command.Context, Display: &code.TurnInputDisplay{Text: command.Text, Files: command.Files, Images: command.Images, Source: command.Message}}
		if _, canCompact := b.agent.(code.Compactor); canCompact && isCompactCommand(command.Text) {
			input.Compact = true
			if focus := compactFocus(command.Text); focus != "" {
				input.Content = []harness.Content{{Text: focus}}
			}
		} else {
			input.Content = b.buildInput(sid, command)
		}
		// A queue recovered after a restart waits for the user; sending continues
		// it, after the input when that input must start now.
		if command.Type == "send" && !command.wait && !command.requireIdle && b.turns.Snapshot(sid).Paused {
			b.turns.Resume(sid)
		}
		var err error
		switch {
		case command.Type == "queue_update":
			err = b.turns.ReplaceQueued(sid, id, input)
		case command.requireIdle:
			_, err = b.turns.Start(b.ctx, sid, input)
		default:
			_, err = b.turns.Submit(b.ctx, sid, input)
		}
		if err != nil {
			return err
		}
		b.ensureTaskPump(sid)
	case "queue_remove":
		if err := b.turns.RemoveQueued(sid, command.InputID); err != nil {
			return err
		}
	case "queue_reorder":
		if err := b.turns.ReorderQueued(sid, command.Order); err != nil {
			return err
		}
	case "cancel":
		if command.InputID != "" {
			if err := b.turns.CancelTurn(sid, command.InputID, command.ClearQueue); err != nil {
				return err
			}
		} else if command.ClearQueue {
			if err := b.turns.CancelAll(sid); err != nil {
				return err
			}
		} else {
			if err := b.turns.CancelCurrent(sid); err != nil {
				return err
			}
		}
	case "settings":
		if command.sync {
			if err := c.wantedSettings(&command); err != nil {
				return err
			}
		}
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
	case "truncate":
		truncator, ok := b.agent.(code.SessionTruncator)
		if !ok {
			return errors.ErrUnsupported
		}
		if command.InputID != "" && !c.cuttable(command.InputID) {
			return errors.New("history can only be cut after a turn of the conversation")
		}
		// What runs or waits belongs to the history that goes.
		if err := b.turns.StopSession(b.ctx, sid); err != nil {
			return err
		}
		if err := truncator.TruncateSession(b.ctx, sid, command.InputID); err != nil {
			return err
		}
		kept := len(transcriptEntries(b.agent.Messages(sid)))
		c.mu.Lock()
		c.hostTurns = slices.DeleteFunc(c.hostTurns, func(turn hostTurn) bool { return turn.at >= kept })
		// The outcome of the last turn stays with it.
		if c.lastInput != command.InputID {
			c.lastOutcome, c.lastInput, c.state.Error = "", "", nil
		}
		c.mu.Unlock()
		c.refreshSettings() // the usage of the dropped turns goes with them
		c.replaceHistory()
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
	default:
		return fmt.Errorf("unknown session command %q", command.Type)
	}
	b.sendTurnSnapshot(sid)
	return nil
}

func (b *backendRuntime) onToolProgress(ctx context.Context, callID, text string) {
	if sid := code.SessionIDFromContext(ctx); sid != "" {
		b.sendSession(sid, Frame{Type: EvtToolProgress, ID: callID, Text: text})
	}
}
