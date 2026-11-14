package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

type behavior struct {
	ModelRequests int `json:"model_requests"`
	// The rest belong to subagents and helper calls.
	LeadRequests      int              `json:"lead_requests"`
	FailedRequests    int              `json:"failed_requests,omitempty"`
	InputTokens       int64            `json:"input_tokens"`
	CachedReadTokens  int64            `json:"cached_read_tokens"`
	OutputTokens      int64            `json:"output_tokens"`
	InstructionsBytes int              `json:"instructions_bytes"`
	ToolSchemaBytes   int              `json:"tool_schema_bytes"`
	ToolCalls         map[string]int   `json:"tool_calls,omitempty"`
	ToolErrors        map[string]int   `json:"tool_errors,omitempty"`
	Signals           map[string]int   `json:"signals,omitempty"`
	Requests          []requestSummary `json:"requests,omitempty"`
	Calls             []toolCallRecord `json:"calls,omitempty"`
}

type requestSummary struct {
	AtMS         int64    `json:"at_ms"`
	DurationMS   int64    `json:"duration_ms"`
	Status       int      `json:"status"`
	Lead         bool     `json:"lead"`
	InputTokens  int64    `json:"input_tokens"`
	CachedTokens int64    `json:"cached_read_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	Calls        []string `json:"calls,omitempty"`
}

type toolCallRecord struct {
	AtMS   int64  `json:"at_ms"`
	Lead   bool   `json:"lead"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Args   string `json:"args"`
	Output string `json:"output,omitempty"`
	Error  bool   `json:"error,omitempty"`
}

const maxRecordedCallText = 4000

var shellFileOp = regexp.MustCompile(`(^|[;&|(\s])(git\s+(mv|rm)|mv|rm|rmdir|unlink)\s`)

// Codex sends its tools as input items, so without a tool-bearing request the first one leads.
func leadConversation(exchanges []recordedExchange) string {
	first := ""
	for _, exchange := range exchanges {
		request := parseRequest(exchange.Request)
		if !request.inference {
			continue
		}
		if request.toolCount > 0 {
			return request.conversationKey
		}
		if first == "" {
			first = request.conversationKey
		}
	}
	return first
}

func analyzeBehavior(exchanges []recordedExchange, leadKey string) behavior {
	result := behavior{ToolCalls: map[string]int{}, ToolErrors: map[string]int{}, Signals: map[string]int{}}
	calls := map[string]*toolCallRecord{}
	var order []string
	outputs := map[string]string{}
	outputErrors := map[string]bool{}

	for _, exchange := range exchanges {
		request := parseRequest(exchange.Request)
		if !request.inference {
			continue
		}
		lead := request.conversationKey == leadKey
		if lead && result.InstructionsBytes == 0 {
			result.InstructionsBytes = request.instructionsBytes
			result.ToolSchemaBytes = request.toolSchemaBytes
		}
		response := parseResponse(exchange.Response)

		summary := requestSummary{
			AtMS: exchange.AtMS, DurationMS: exchange.DurationMS, Status: exchange.Status, Lead: lead,
			InputTokens: response.inputTokens, CachedTokens: response.cachedTokens, OutputTokens: response.outputTokens,
		}
		result.ModelRequests++
		if lead {
			result.LeadRequests++
		}
		if exchange.Status >= 400 {
			result.FailedRequests++
		}
		result.InputTokens += response.inputTokens
		result.CachedReadTokens += response.cachedTokens
		result.OutputTokens += response.outputTokens

		for _, call := range response.calls {
			summary.Calls = append(summary.Calls, call.Name)
			if _, seen := calls[call.ID]; seen || call.ID == "" {
				continue
			}
			record := call
			record.AtMS, record.Lead = exchange.AtMS, lead
			calls[call.ID] = &record
			order = append(order, call.ID)
		}
		for id, output := range request.outputs {
			outputs[id] = output.text
			if output.failed {
				outputErrors[id] = true
			}
		}
		result.Requests = append(result.Requests, summary)
	}

	for _, id := range order {
		call := calls[id]
		call.Output = clip(outputs[id], maxRecordedCallText)
		call.Error = outputErrors[id] || looksLikeToolError(outputs[id])
		result.ToolCalls[call.Name]++
		if call.Error {
			result.ToolErrors[call.Name]++
		}
		addSignals(result.Signals, *call, outputs[id])
		call.Args = clip(call.Args, maxRecordedCallText)
		result.Calls = append(result.Calls, *call)
	}
	return result
}

func addSignals(signals map[string]int, call toolCallRecord, output string) {
	var args map[string]any
	_ = json.Unmarshal([]byte(call.Args), &args)

	switch call.Name {
	case "edit":
		entries, _ := args["edits"].([]any)
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			if move, _ := entry["move_to"].(string); strings.TrimSpace(move) != "" {
				signals["edit_move"]++
			}
			if remove, _ := entry["delete"].(bool); remove {
				signals["edit_delete"]++
			}
		}
	case "exec", "exec_command", "Bash", "shell", "local_shell", "shell_command":
		command, _ := args["command"].(string)
		if command == "" {
			command, _ = args["cmd"].(string)
		}
		if shellFileOp.MatchString(command) {
			signals["shell_file_op"]++
		}
	case "agent":
		if background, _ := args["background"].(bool); background {
			signals["agent_background"]++
		}
		if fork, _ := args["fork_context"].(bool); fork {
			signals["agent_fork_context"]++
		}
	}
	if strings.Contains(output, "plan mode only allows read-only tool calls") {
		signals["plan_blocked"]++
	}
	if strings.Contains(output, "unattended mode is active") {
		signals["unattended_blocked"]++
	}
}

func looksLikeToolError(output string) bool {
	trimmed := strings.TrimSpace(output)
	return strings.HasPrefix(trimmed, "error:") || strings.HasPrefix(trimmed, "Error:")
}

func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

type parsedRequest struct {
	inference         bool
	conversationKey   string
	instructionsBytes int
	toolSchemaBytes   int
	toolCount         int
	outputs           map[string]parsedOutput
}

type parsedOutput struct {
	text   string
	failed bool
}

func parseRequest(raw json.RawMessage) parsedRequest {
	var body struct {
		Instructions string            `json:"instructions"`
		System       json.RawMessage   `json:"system"`
		Tools        []json.RawMessage `json:"tools"`
		Input        json.RawMessage   `json:"input"`
		Messages     []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	parsed := parsedRequest{outputs: map[string]parsedOutput{}}
	if json.Unmarshal(raw, &body) != nil {
		return parsed
	}
	instructions := body.Instructions
	if instructions == "" && len(body.System) > 0 {
		instructions = flattenText(body.System)
	}

	var items []map[string]json.RawMessage
	_ = json.Unmarshal(body.Input, &items)
	for _, item := range items {
		kind := rawString(item["type"])
		switch kind {
		case "function_call_output", "custom_tool_call_output":
			parsed.outputs[rawString(item["call_id"])] = parsedOutput{text: flattenText(item["output"])}
		case "message":
			// Codex sends its base prompt as the first developer message.
			if instructions == "" && rawString(item["role"]) == "developer" {
				instructions = flattenText(item["content"])
			}
		}
	}
	for _, message := range body.Messages {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if rawString(block["type"]) != "tool_result" {
				continue
			}
			failed := false
			_ = json.Unmarshal(block["is_error"], &failed)
			parsed.outputs[rawString(block["tool_use_id"])] = parsedOutput{text: flattenText(block["content"]), failed: failed}
		}
	}

	parsed.inference = len(items) > 0 || len(body.Messages) > 0
	parsed.instructionsBytes = len(instructions)
	parsed.toolCount = len(body.Tools)
	for _, tool := range body.Tools {
		parsed.toolSchemaBytes += len(tool)
	}
	// A mode appends to the prompt, so only its opening identifies the conversation.
	digest := sha256.Sum256([]byte(instructions[:min(len(instructions), 256)]))
	parsed.conversationKey = hex.EncodeToString(digest[:8])
	return parsed
}

type parsedResponse struct {
	inputTokens  int64
	cachedTokens int64
	outputTokens int64
	calls        []toolCallRecord
}

func parseResponse(raw string) parsedResponse {
	var parsed parsedResponse
	blocks := map[int]*toolCallRecord{}
	var blockOrder []int

	handle := func(data []byte) {
		var event struct {
			Type     string `json:"type"`
			Index    int    `json:"index"`
			Response *struct {
				Output []map[string]json.RawMessage `json:"output"`
				Usage  json.RawMessage              `json:"usage"`
			} `json:"response"`
			Message *struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
			Usage        json.RawMessage              `json:"usage"`
			ContentBlock map[string]json.RawMessage   `json:"content_block"`
			Delta        map[string]json.RawMessage   `json:"delta"`
			Output       []map[string]json.RawMessage `json:"output"`
		}
		if json.Unmarshal(data, &event) != nil {
			return
		}
		switch event.Type {
		case "response.completed", "response.incomplete", "response.failed":
			if event.Response == nil {
				return
			}
			parsed.addResponsesUsage(event.Response.Usage)
			parsed.calls = append(parsed.calls, responsesCalls(event.Response.Output)...)
		case "message_start":
			if event.Message != nil {
				parsed.addMessagesUsage(event.Message.Usage)
			}
		case "message_delta":
			parsed.addMessagesUsage(event.Usage)
		case "content_block_start":
			if rawString(event.ContentBlock["type"]) == "tool_use" {
				blocks[event.Index] = &toolCallRecord{ID: rawString(event.ContentBlock["id"]), Name: rawString(event.ContentBlock["name"])}
				blockOrder = append(blockOrder, event.Index)
			}
		case "content_block_delta":
			if block := blocks[event.Index]; block != nil && rawString(event.Delta["type"]) == "input_json_delta" {
				block.Args += rawString(event.Delta["partial_json"])
			}
		case "":
			// Non-streamed Responses body.
			if len(event.Output) > 0 || len(event.Usage) > 0 {
				parsed.addResponsesUsage(event.Usage)
				parsed.calls = append(parsed.calls, responsesCalls(event.Output)...)
			}
		}
	}

	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		handle([]byte(trimmed))
	} else {
		for line := range strings.SplitSeq(raw, "\n") {
			if data, ok := strings.CutPrefix(line, "data:"); ok {
				handle([]byte(strings.TrimSpace(data)))
			}
		}
	}
	for _, index := range blockOrder {
		parsed.calls = append(parsed.calls, *blocks[index])
	}
	return parsed
}

func responsesCalls(output []map[string]json.RawMessage) []toolCallRecord {
	var calls []toolCallRecord
	for _, item := range output {
		switch rawString(item["type"]) {
		case "function_call":
			calls = append(calls, toolCallRecord{ID: rawString(item["call_id"]), Name: rawString(item["name"]), Args: rawString(item["arguments"])})
		case "custom_tool_call":
			calls = append(calls, toolCallRecord{ID: rawString(item["call_id"]), Name: rawString(item["name"]), Args: rawString(item["input"])})
		case "local_shell_call":
			calls = append(calls, toolCallRecord{ID: rawString(item["call_id"]), Name: "local_shell", Args: string(item["action"])})
		}
	}
	return calls
}

func (p *parsedResponse) addResponsesUsage(raw json.RawMessage) {
	var usage struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return
	}
	p.inputTokens += usage.InputTokens
	p.cachedTokens += usage.InputTokensDetails.CachedTokens
	p.outputTokens += usage.OutputTokens
}

// Anthropic repeats cumulative usage in message_delta, so keep the largest of each.
func (p *parsedResponse) addMessagesUsage(raw json.RawMessage) {
	var usage struct {
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
		CacheReadTokens  int64 `json:"cache_read_input_tokens"`
		CacheWriteTokens int64 `json:"cache_creation_input_tokens"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return
	}
	p.inputTokens = max(p.inputTokens, usage.InputTokens+usage.CacheReadTokens+usage.CacheWriteTokens)
	p.cachedTokens = max(p.cachedTokens, usage.CacheReadTokens)
	p.outputTokens = max(p.outputTokens, usage.OutputTokens)
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// Image parts are noted so an image-only result is not read as empty.
func flattenText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, part := range parts {
		switch {
		case len(part["text"]) > 0:
			out = append(out, rawString(part["text"]))
		case strings.Contains(rawString(part["type"]), "image"):
			out = append(out, "[image]")
		}
	}
	return strings.Join(out, "\n")
}
