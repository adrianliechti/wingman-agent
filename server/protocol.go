package server

const (
	MsgSend   = "send"
	MsgCancel = "cancel"
)

type ClientMessage struct {
	Type      string   `json:"type"`
	SessionID string   `json:"session,omitempty"`
	Text      string   `json:"text,omitempty"`
	Files     []string `json:"files,omitempty"`
	Images    []string `json:"images,omitempty"` // base64 data URLs, e.g. "data:image/png;base64,..."
}

const (
	EvtSessionState        = "session_state"
	EvtTextDelta           = "text_delta"
	EvtReasoningDelta      = "reasoning_delta"
	EvtToolCall            = "tool_call"
	EvtToolResult          = "tool_result"
	EvtPhase               = "phase"
	EvtUsage               = "usage"
	EvtError               = "error"
	EvtFilesChanged        = "files_changed"
	EvtDiffsChanged        = "diffs_changed"
	EvtCheckpointsChanged  = "checkpoints_changed"
	EvtSessionsChanged     = "sessions_changed"
	EvtDiagnosticsChanged  = "diagnostics_changed"
	EvtCapabilitiesChanged = "capabilities_changed"
)

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

type ConversationMessage struct {
	Role    string                `json:"role"`
	Content []ConversationContent `json:"content"`
}

type ConversationContent struct {
	Text       string                 `json:"text,omitempty"`
	Image      *ConversationImage     `json:"image,omitempty"`
	Reasoning  *ConversationReasoning `json:"reasoning,omitempty"`
	ToolCall   *ConversationTool      `json:"tool_call,omitempty"`
	ToolResult *ConversationResult    `json:"tool_result,omitempty"`
}

type ConversationImage struct {
	Data string `json:"data"` // base64 data URL
	Name string `json:"name,omitempty"`
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
	Status   string `json:"status"`
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
