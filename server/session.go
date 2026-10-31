package server

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/tui"
	"github.com/google/uuid"
)

// TranscriptEntry is the visible Wingman model. Provider messages, retry
// bookkeeping, hidden instructions and journal events never cross this boundary.
type TranscriptEntry struct {
	ID            string                 `json:"id"`
	Type          string                 `json:"type"`
	Content       string                 `json:"content"`
	MessageID     string                 `json:"messageId,omitempty"`
	InputID       string                 `json:"inputId,omitempty"`
	Files         []string               `json:"files,omitempty"`
	Images        []string               `json:"images,omitempty"`
	ToolID        string                 `json:"toolId,omitempty"`
	ToolName      string                 `json:"toolName,omitempty"`
	ToolKind      string                 `json:"toolKind,omitempty"`
	ToolArgs      string                 `json:"toolArgs,omitempty"`
	ToolLocations []harness.ToolLocation `json:"toolLocations,omitempty"`
	ToolHint      string                 `json:"toolHint,omitempty"`
	ToolResult    *string                `json:"toolResult,omitempty"`
	ToolPartial   bool                   `json:"toolPartial,omitempty"`
	ReasoningID   string                 `json:"reasoningId,omitempty"`
}

type PromptView struct {
	ID      string             `json:"id"`
	Kind    string             `json:"kind"`
	Message string             `json:"message"`
	Fields  []tool.ElicitField `json:"fields,omitempty"`
}

type SessionSettings struct {
	Models     []modelOption `json:"models"`
	Model      string        `json:"model"`
	Effort     string        `json:"effort"`
	Efforts    []string      `json:"efforts"`
	Mode       string        `json:"mode"`
	Modes      []modeOption  `json:"modes"`
	Unattended *bool         `json:"unattended,omitempty"`
	CanFork    bool          `json:"canFork"`
	CanCompact bool          `json:"canCompact"`
}

type modelOption struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Namespace string   `json:"namespace"`
	Efforts   []string `json:"efforts,omitempty"`
}

type UsageView struct {
	InputTokens     int64 `json:"inputTokens"`
	CachedTokens    int64 `json:"cachedTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	LastInputTokens int64 `json:"lastInputTokens"`
	ContextWindow   int64 `json:"contextWindow"`
}

type SessionState struct {
	Status        string             `json:"status"`
	Phase         string             `json:"phase"`
	Retry         *harness.RetryInfo `json:"retry,omitempty"`
	Error         *string            `json:"error"`
	Usage         UsageView          `json:"usage"`
	Prompts       []PromptView       `json:"prompts"`
	PendingInputs []TurnQueueEntry   `json:"pendingInputs"`
	CanSteer      bool               `json:"canSteer"`
	Settings      SessionSettings    `json:"settings"`
	ToolProgress  map[string]string  `json:"toolProgress"`
}

type SessionChange struct {
	Type    string
	Entry   *TranscriptEntry
	Entries []TranscriptEntry
	ID      string
	Text    string
	IDs     []string
	State   *SessionState
}

type sessionController struct {
	metadataMu sync.Mutex
	receiptMu  sync.Mutex
	backend    *backendRuntime
	ref        SessionRef
	epoch      string
	// opMu serializes load and commands, never stream callbacks.
	opMu           sync.Mutex
	loaded         bool
	deleted        bool
	receipts       map[string]*commandReceipt
	deliveredTasks map[string]bool
	// mu protects the complete projection AND subscription registration/publication.
	mu          sync.Mutex
	entries     []TranscriptEntry
	state       SessionState
	prompts     map[string]pendingPrompt
	textID      string
	reasoningID string
	attempt     map[string]bool
	activeInput string
	lastOutcome string
	compactions []compactionTurn
	ahp         *ahpChat
}

// compactionTurn keeps a compaction turn, which the agent's history does not
// record, at its place in the transcript.
type compactionTurn struct {
	at      int // history entries before it
	entries []TranscriptEntry
}

func newSessionController(b *backendRuntime, id string) *sessionController {
	return &sessionController{backend: b, ref: SessionRef{b.scope.WorkspaceID, b.id, id}, epoch: uuid.NewString(), receipts: map[string]*commandReceipt{}, deliveredTasks: map[string]bool{}, entries: []TranscriptEntry{}, prompts: map[string]pendingPrompt{}, attempt: map[string]bool{}, state: SessionState{Status: "loading", Phase: "idle", Prompts: []PromptView{}, PendingInputs: []TurnQueueEntry{}, ToolProgress: map[string]string{}, Settings: SessionSettings{Models: []modelOption{}, Efforts: []string{}, Modes: []modeOption{}}}}
}

func (c *sessionController) load() {
	if !c.backend.beginOperation() {
		return
	}
	defer c.backend.operations.Done()
	c.opMu.Lock()
	defer c.opMu.Unlock()
	_ = c.loadLocked()
}

func (c *sessionController) loadLocked() error {
	if err := c.backend.ctx.Err(); err != nil {
		return err
	}
	if c.deleted {
		return errors.New("session deleted")
	}
	if c.loaded {
		return nil
	}
	err := c.backend.agent.LoadSession(code.WithSessionID(c.backend.ctx, c.ref.SessionID), c.ref.SessionID)
	if err != nil {
		c.mu.Lock()
		message := err.Error()
		c.state.Status = "error"
		c.state.Error = &message
		c.publishStateLocked()
		c.mu.Unlock()
		return err
	}
	c.loaded = true
	c.replaceHistory()
	c.refreshSettings()
	c.mu.Lock()
	c.state.Status = "ready"
	c.state.Error = nil
	c.publishStateLocked()
	c.mu.Unlock()
	c.backend.sendTurnSnapshot(c.ref.SessionID)
	c.backend.ensureTaskPump(c.ref.SessionID)
	return nil
}

func (b *backendRuntime) settings(sid string) SessionSettings {
	models, model := b.agent.Models(sid)
	effort, efforts := b.agent.Effort(sid)
	modes, mode := b.agent.Modes(sid)
	_, canFork := b.agent.(code.SessionForker)
	_, canCompact := b.agent.(code.Compactor)
	state := SessionSettings{Model: model, Effort: effort, Efforts: append([]string{}, efforts...), Mode: mode, Modes: toModeState(modes, mode).Modes, Models: []modelOption{}, CanFork: canFork, CanCompact: canCompact}
	if policy, ok := b.agent.(code.UnattendedAgent); ok {
		enabled := policy.Unattended(sid)
		state.Unattended = &enabled
	}
	for _, m := range models {
		state.Models = append(state.Models, modelOption{ID: m.ID, Name: m.Name, Namespace: m.Namespace, Efforts: slices.Clone(m.Efforts)})
	}
	return state
}

func (c *sessionController) refreshSettings() {
	if !c.backend.beginOperation() {
		return
	}
	defer c.backend.operations.Done()
	c.metadataMu.Lock()
	defer c.metadataMu.Unlock()
	settings := c.backend.settings(c.ref.SessionID)
	usage := c.backend.agent.Usage(c.ref.SessionID)
	if usage.ContextWindow <= 0 && usage.LastInputTokens > 0 {
		usage.ContextWindow = int64(harness.ContextWindowFor(settings.Model))
	}
	u := UsageView{usage.InputTokens, usage.CacheReadInputTokens, usage.OutputTokens, usage.LastInputTokens, usage.ContextWindow}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Status == "deleted" {
		return
	}
	if reflect.DeepEqual(c.state.Settings, settings) && c.state.Usage == u {
		return
	}
	c.state.Settings = settings
	c.state.Usage = u
	c.publishStateLocked()
}

func (c *sessionController) publishStateLocked() {
	c.publishLocked(SessionChange{Type: "state.replace", State: &c.state})
}

func (c *sessionController) replaceHistory() {
	entries := transcriptEntries(c.backend.agent.Messages(c.ref.SessionID))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Status == "deleted" {
		return
	}
	inserted := 0
	for _, turn := range c.compactions {
		at := min(turn.at+inserted, len(entries))
		entries = slices.Insert(entries, at, turn.entries...)
		inserted += len(turn.entries)
	}
	c.entries = entries
	c.textID = ""
	c.reasoningID = ""
	c.attempt = map[string]bool{}
	// Explicit authoritative replacement at load/commit boundaries. No client
	// tries to match streamed text against history by content or timing.
	c.publishLocked(SessionChange{Type: "entries.replace", Entries: entries})
}

func (b *backendRuntime) sendSession(sid string, f Frame) {
	if sid == "" {
		return
	}
	if f.Type == EvtTasksChanged {
		f.Session = sid
		f.Backend = b.id
		b.Server.send(f)
		return
	}
	b.session(sid).apply(f)
}

func (b *backendRuntime) setSessionPhase(sid, phase string) {
	b.sendSession(sid, Frame{Type: EvtPhase, Phase: phase})
}

func (c *sessionController) compactedLocked(inputID string, result harness.CompactResult) {
	notice := TranscriptEntry{ID: "notice:" + inputID, Type: "notice", Content: fmt.Sprintf("Context compacted · ~%s → ~%s tokens", tui.FormatTokens(result.TokensBefore), tui.FormatTokens(result.TokensAfter))}
	if n := len(c.compactions); n > 0 && c.compactions[n-1].entries[0].InputID == inputID {
		c.compactions[n-1].entries = append(c.compactions[n-1].entries, notice)
	}
	c.upsertLocked(notice)
}

func (c *sessionController) index(id string) int {
	return slices.IndexFunc(c.entries, func(e TranscriptEntry) bool { return e.ID == id })
}

func (c *sessionController) upsertLocked(e TranscriptEntry) {
	if i := c.index(e.ID); i >= 0 {
		c.entries[i] = e
	} else {
		c.entries = append(c.entries, e)
	}
	c.publishLocked(SessionChange{Type: "entry.upsert", Entry: &e})
}

func (c *sessionController) apply(f Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Status == "deleted" {
		return
	}
	switch f.Type {
	case EvtPhase:
		if c.state.Phase == f.Phase {
			return
		}
		c.state.Phase = f.Phase
		if f.Phase != "retrying" {
			c.state.Retry = nil
		}
	case EvtError:
		c.state.Error = &f.Message
	case EvtToolProgress:
		c.state.ToolProgress[f.ID] = f.Text
	case EvtTurnInput:
		if f.Input != nil {
			switch code.TurnInputState(f.Input.State) {
			case code.TurnInputActive:
				c.activeInput, c.lastOutcome = f.Input.ID, ""
			case code.TurnInputCompleted, code.TurnInputIncomplete, code.TurnInputFailed, code.TurnInputCancelled:
				if c.activeInput == f.Input.ID {
					c.activeInput, c.lastOutcome = "", f.Input.State
				}
			}
		}
		if f.Input != nil && (f.Input.State == string(code.TurnInputActive) || f.Input.State == string(code.TurnInputSteered)) {
			c.state.Error = nil
			if f.Input.Origin != "task" && (f.Input.Text != "" || len(f.Input.Files)+len(f.Input.Images) > 0) {
				entry := TranscriptEntry{ID: "input:" + f.Input.ID, InputID: f.Input.ID, Type: "user", Content: f.Input.Text, Files: f.Input.Files, Images: f.Input.Images}
				c.upsertLocked(entry)
				if f.Input.Compact && c.index(entry.ID) >= 0 && !slices.ContainsFunc(c.compactions, func(turn compactionTurn) bool { return turn.entries[0].ID == entry.ID }) {
					at := c.index(entry.ID)
					for _, turn := range c.compactions {
						at -= len(turn.entries)
					}
					c.compactions = append(c.compactions, compactionTurn{at: max(0, at), entries: []TranscriptEntry{entry}})
				}
			}
			c.textID = ""
			c.reasoningID = ""
		}
	case EvtTextDelta, EvtReasoningDelta:
		typ, current := "assistant", &c.textID
		if f.Type == EvtReasoningDelta {
			typ = "reasoning"
			current = &c.reasoningID
		}
		i := c.index(*current)
		if i < 0 || (f.ID != "" && c.entries[i].MessageID != f.ID) {
			id := uuid.NewString()
			if f.ID != "" {
				id = typ + ":" + f.ID
			}
			e := TranscriptEntry{ID: id, Type: typ, Content: f.Text, MessageID: f.ID}
			if typ == "reasoning" {
				e.ReasoningID = f.ID
			}
			*current = e.ID
			c.attempt[e.ID] = true
			c.upsertLocked(e)
		} else {
			c.entries[i].Content += f.Text
			c.publishLocked(SessionChange{Type: "entry.append", ID: c.entries[i].ID, Text: f.Text})
		}
		return
	case EvtImage:
		if f.Content == "" {
			return
		}
		c.textID = ""
		c.reasoningID = ""
		c.upsertLocked(TranscriptEntry{ID: uuid.NewString(), Type: "assistant", Images: []string{f.Content}})
		return
	case EvtToolCall, EvtToolResult:
		c.textID = ""
		c.reasoningID = ""
		id := "tool:" + f.ID
		if f.ID == "" {
			id = uuid.NewString()
		} // adapters should supply IDs; never match by text/name
		e := TranscriptEntry{ID: id, Type: "tool", ToolID: f.ID, ToolName: f.Name, ToolKind: f.Kind, ToolArgs: f.Args, ToolLocations: f.Locations, ToolHint: f.Hint, ToolPartial: f.Partial}
		if i := c.index(id); i >= 0 {
			e.ToolResult = c.entries[i].ToolResult
		}
		if f.Type == EvtToolResult {
			e.ToolResult = &f.Content
			delete(c.state.ToolProgress, f.ID)
		}
		c.upsertLocked(e)
	case EvtStreamReset:
		ids := []string{}
		c.entries = slices.DeleteFunc(c.entries, func(e TranscriptEntry) bool {
			if c.attempt[e.ID] || (e.Type == "tool" && e.ToolPartial) {
				ids = append(ids, e.ID)
				return true
			}
			return false
		})
		c.attempt = map[string]bool{}
		c.textID = ""
		c.reasoningID = ""
		c.publishLocked(SessionChange{Type: "entries.remove", IDs: ids})
		return
	default:
		return
	}
	c.publishStateLocked()
}

func visibleInput(input code.TurnInput) code.TurnInputDisplay {
	if input.Display != nil {
		return *input.Display
	}
	// Legacy disk reader: infer the original visible envelope once, at the Go
	// boundary. Hidden task/skill content is never rendered as user input.
	var out code.TurnInputDisplay
	var texts []string
	for _, part := range input.Content {
		if part.Hidden {
			continue
		}
		if strings.HasPrefix(part.Text, "[File: ") && strings.HasSuffix(part.Text, "]") && !strings.Contains(part.Text, "\n") {
			out.Files = append(out.Files, strings.TrimSuffix(strings.TrimPrefix(part.Text, "[File: "), "]"))
		} else if part.Text != "" {
			texts = append(texts, part.Text)
		}
		if part.File != nil && part.File.Data != "" {
			if part.File.Name != "" && slices.Contains(out.Files, part.File.Name) {
				continue
			}
			out.Images = append(out.Images, part.File.Data)
		}
	}
	out.Text = strings.Join(texts, "\n")
	return out
}

func transcriptEntries(messages []harness.Message) []TranscriptEntry {
	entries := []TranscriptEntry{}
	for mi, m := range convertMessages(messages) {
		if m.Role == "user" {
			input := code.TurnInput{}
			for _, part := range m.Content {
				if part.Text != "" {
					input.Content = append(input.Content, harness.Content{Text: part.Text})
				}
				if part.Image != nil {
					input.Content = append(input.Content, harness.Content{File: &harness.File{Data: part.Image.Data}})
				}
			}
			v := visibleInput(input)
			if v.Text != "" || len(v.Files)+len(v.Images) > 0 {
				id := fmt.Sprintf("history:%d:user", mi)
				if m.InputID != "" {
					id = "input:" + m.InputID
				}
				entries = append(entries, TranscriptEntry{ID: id, InputID: m.InputID, Type: "user", Content: v.Text, Files: v.Files, Images: v.Images})
			}
			continue
		}
		for ci, part := range m.Content {
			id := fmt.Sprintf("history:%d:%d", mi, ci)
			if part.Text != "" {
				textID := id + ":text"
				if part.TextID != "" {
					textID = "assistant:" + part.TextID
				}
				entries = append(entries, TranscriptEntry{ID: textID, Type: "assistant", Content: part.Text, MessageID: part.TextID})
			}
			if part.Image != nil && part.Image.Data != "" {
				entries = append(entries, TranscriptEntry{ID: id + ":image", Type: "assistant", Images: []string{part.Image.Data}})
			}
			if part.Reasoning != nil {
				reasonID := id + ":reasoning"
				if part.Reasoning.ID != "" {
					reasonID = "reasoning:" + part.Reasoning.ID
				}
				entries = append(entries, TranscriptEntry{ID: reasonID, Type: "reasoning", Content: part.Reasoning.Summary, ReasoningID: part.Reasoning.ID})
			}
			if t := part.ToolCall; t != nil {
				tid := "tool:" + t.ID
				if t.ID == "" {
					tid = id + ":tool"
				}
				entries = append(entries, TranscriptEntry{ID: tid, Type: "tool", ToolID: t.ID, ToolName: t.Name, ToolKind: t.Kind, ToolArgs: t.Args, ToolLocations: t.Locations, ToolHint: t.Hint})
			}
			if t := part.ToolResult; t != nil {
				i := slices.IndexFunc(entries, func(e TranscriptEntry) bool { return t.ID != "" && e.ToolID == t.ID })
				if i >= 0 {
					entries[i].ToolResult = &t.Content
					continue
				}
				entries = append(entries, TranscriptEntry{ID: id + ":result", Type: "tool", ToolID: t.ID, ToolName: t.Name, ToolKind: t.Kind, ToolArgs: t.Args, ToolLocations: t.Locations, ToolResult: &t.Content})
			}
		}
	}
	return entries
}
