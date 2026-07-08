package agent

type ModelInfo struct {
	ID string
}

type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleSystem    MessageRole = "system"
)

type Message struct {
	Role MessageRole `json:"role"`

	Content []Content `json:"content"`
	Hidden  bool      `json:"hidden,omitempty"`
}

type Content struct {
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`

	File *File `json:"file,omitempty"`

	Reasoning *Reasoning `json:"reasoning,omitempty"`

	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

type File struct {
	Name string `json:"name,omitempty"`
	Data string `json:"data,omitempty"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	// LastInputTokens is the input size of the most recent request — the
	// current context occupancy, unlike the cumulative counters above.
	LastInputTokens int64 `json:"last_input_tokens,omitempty"`
}

type ToolCall struct {
	ID string `json:"id"`

	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

type ToolResult struct {
	ID string `json:"id,omitempty"`

	Name string `json:"name"`
	Args string `json:"args,omitempty"`

	Content string `json:"content,omitempty"`
}

type Reasoning struct {
	ID string `json:"id,omitempty"`

	Summary string `json:"summary,omitempty"`

	// Content is the provider's opaque (encrypted) reasoning payload, only
	// replayable to the model that produced it. Model tags the producer so the
	// agent loop can purge stale payloads when the session model changes.
	Content string `json:"content,omitempty"`
	Model   string `json:"model,omitempty"`
}
