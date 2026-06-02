package claude

import (
	"encoding/json"

	"github.com/coder/acp-go-sdk"
)

// mcpConfigJSON renders ACP MCP servers as the inline JSON the CLI's
// --mcp-config flag accepts ({"mcpServers":{name:{...}}}). Returns "" when
// there are none. stdio/http/sse transports are supported.
func mcpConfigJSON(servers []acp.McpServer) string {
	if len(servers) == 0 {
		return ""
	}
	out := map[string]map[string]any{}
	for _, s := range servers {
		switch {
		case s.Stdio != nil:
			env := map[string]string{}
			for _, e := range s.Stdio.Env {
				env[e.Name] = e.Value
			}
			out[s.Stdio.Name] = map[string]any{"type": "stdio", "command": s.Stdio.Command, "args": s.Stdio.Args, "env": env}
		case s.Http != nil:
			out[s.Http.Name] = map[string]any{"type": "http", "url": s.Http.Url, "headers": headerMap(s.Http.Headers)}
		case s.Sse != nil:
			out[s.Sse.Name] = map[string]any{"type": "sse", "url": s.Sse.Url, "headers": headerMap(s.Sse.Headers)}
		}
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.Marshal(map[string]any{"mcpServers": out})
	if err != nil {
		return ""
	}
	return string(b)
}

func headerMap(headers []acp.HttpHeader) map[string]string {
	m := map[string]string{}
	for _, h := range headers {
		m[h.Name] = h.Value
	}
	return m
}

// Wire types for `claude --output-format=stream-json` / `--input-format=stream-json`.
// Each line of output is one JSON object; `type` discriminates. We only model
// the fields we consume — unknown fields are tolerated and dropped by
// encoding/json.

type cliEnvelope struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
	Event   json.RawMessage `json:"event,omitempty"` // stream_event payload (partial messages)
}

// streamEvent is one partial-message event emitted under --include-partial-messages.
// We act only on content_block_delta text/thinking deltas to stream the
// assistant's reply token-by-token; the other event types are no-ops here
// (tool_use still arrives via the full assistant message).
type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		Thinking string `json:"thinking,omitempty"`
	} `json:"delta"`
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

// cliResult is the terminating `result` line of a turn.
type cliResult struct {
	Subtype      string                   `json:"subtype"`
	StopReason   string                   `json:"stop_reason"`
	IsError      bool                     `json:"is_error"`
	Result       string                   `json:"result"`
	Errors       []string                 `json:"errors"`
	Usage        *cliUsage                `json:"usage,omitempty"`
	ModelUsage   map[string]cliModelUsage `json:"modelUsage,omitempty"`
	TotalCostUSD float64                  `json:"total_cost_usd,omitempty"`
}

type cliUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type cliModelUsage struct {
	ContextWindow int `json:"contextWindow"`
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
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Source *cliImageSource `json:"source,omitempty"`
}

type cliImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
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
