package server

// Inbound WebSocket message types. The client tells the server what it
// wants done; the server tags every reply with `session` so the React
// client can route per-session state without an out-of-band registry.
const (
	MsgSend   = "send"
	MsgCancel = "cancel"
)

// ClientMessage is the envelope for every client → server WebSocket frame.
// Inbound shares one struct because the type isn't known until unmarshal.
type ClientMessage struct {
	Type      string   `json:"type"`
	SessionID string   `json:"session,omitempty"`
	Text      string   `json:"text,omitempty"`
	Files     []string `json:"files,omitempty"`
}

// Outbound WebSocket event names.
const (
	EvtSessionState         = "session_state"   // hello: full snapshot for a session
	EvtTextDelta            = "text_delta"      // streaming assistant text
	EvtReasoningDelta       = "reasoning_delta" // streaming chain-of-thought summary
	EvtToolCall             = "tool_call"       // assistant invoking a tool
	EvtToolResult           = "tool_result"     // tool result for a prior call
	EvtPhase                = "phase"           // idle | thinking | streaming | tool_running
	EvtUsage                = "usage"           // token accounting update
	EvtError                = "error"           // turn-level error message
	EvtFilesChanged         = "files_changed"   // workspace file tree dirty
	EvtDiffsChanged         = "diffs_changed"   // workspace diffs dirty
	EvtCheckpointsChanged   = "checkpoints_changed"
	EvtSessionsChanged      = "sessions_changed"
	EvtDiagnosticsChanged   = "diagnostics_changed"
	EvtCapabilitiesChanged  = "capabilities_changed"
)

// Frame is the wire shape for every server → client event. One flat struct
// instead of an interface + 18 typed events: a single Marshal call per
// emit, no envelope-injection dance. Fields are `omitempty` so unused ones
// don't bloat the wire; the React side discriminates on `Type` exactly
// like the previous per-struct design.
type Frame struct {
	Type    string `json:"type"`
	Session string `json:"session,omitempty"`

	Text    string `json:"text,omitempty"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Args    string `json:"args,omitempty"`
	Hint    string `json:"hint,omitempty"`
	Content string `json:"content,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`

	InputTokens  int64 `json:"input_tokens,omitempty"`
	CachedTokens int64 `json:"cached_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`

	Messages []ConversationMessage `json:"messages,omitempty"`
}

// ConversationMessage is the message shape replayed in session_state's
// `messages` field, and returned by /api/sessions/{id}/load.
type ConversationMessage struct {
	Role    string                `json:"role"`
	Content []ConversationContent `json:"content"`
}

type ConversationContent struct {
	Text       string                 `json:"text,omitempty"`
	Reasoning  *ConversationReasoning `json:"reasoning,omitempty"`
	ToolCall   *ConversationTool      `json:"tool_call,omitempty"`
	ToolResult *ConversationResult    `json:"tool_result,omitempty"`
}

type ConversationReasoning struct {
	ID      string `json:"id,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type ConversationTool struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
	Hint string `json:"hint,omitempty"`
}

type ConversationResult struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Args    string `json:"args,omitempty"`
	Content string `json:"content"`
}

// REST payload shapes.

type FileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type FileContent struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Language string `json:"language"`
}

type DiffEntry struct {
	Path     string `json:"path"`
	Status   string `json:"status"` // "added", "modified", "deleted"
	Patch    string `json:"patch"`
	Original string `json:"original,omitempty"`
	Modified string `json:"modified,omitempty"`
	Language string `json:"language,omitempty"`
}

type CheckpointEntry struct {
	Hash    string `json:"hash"`
	Message string `json:"message"`
	Time    string `json:"time"`
}

type SessionEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}
