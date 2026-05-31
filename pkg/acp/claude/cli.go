package claude

import "encoding/json"

// Wire types for `claude --output-format=stream-json` / `--input-format=stream-json`.
// Each line of output is one JSON object; `type` discriminates. We only model
// the fields we consume — unknown fields are tolerated and dropped by
// encoding/json.

type cliEnvelope struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
}

type cliMessage struct {
	Content []cliMsgBlock `json:"content"`
}

// cliMsgBlock is the union of Anthropic content blocks Claude emits. We act on
// text / thinking / tool_use / tool_result and ignore the rest.
type cliMsgBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result body (string or block list)
	IsError   bool            `json:"is_error,omitempty"`
}

// cliInput is the shape we write to claude's stdin when --input-format=stream-json.
type cliInput struct {
	Type    string          `json:"type"`
	Message cliInputMessage `json:"message"`
}

type cliInputMessage struct {
	Role    string            `json:"role"`
	Content []cliInputContent `json:"content"`
}

type cliInputContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Control protocol: with --permission-prompt-tool stdio the CLI emits a
// control_request (subtype "can_use_tool") on stdout when a tool needs
// approval; the client replies with a control_response on stdin.
type controlRequest struct {
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype   string          `json:"subtype"`
		ToolName  string          `json:"tool_name"`
		ToolUseID string          `json:"tool_use_id"`
		Input     json.RawMessage `json:"input"`
		Description string        `json:"description"`
	} `json:"request"`
}

type controlResponse struct {
	Type     string              `json:"type"`
	Response controlResponseBody `json:"response"`
}

// controlInterrupt aborts the in-flight turn while keeping the process alive.
type controlInterrupt struct {
	Type      string               `json:"type"`
	RequestID string               `json:"request_id"`
	Request   controlInterruptBody `json:"request"`
}

type controlInterruptBody struct {
	Subtype string `json:"subtype"`
}

type controlResponseBody struct {
	Subtype   string `json:"subtype"` // success | error
	RequestID string `json:"request_id"`
	Response  any    `json:"response,omitempty"`
	Error     string `json:"error,omitempty"`
}
