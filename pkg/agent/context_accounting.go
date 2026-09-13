package agent

import (
	"crypto/sha256"
	"encoding/json"
)

// Images are model-dependent. Use a modest fallback rather than treating a
// base64 data URL as text; the next response's usage calibrates the full input.
const estimatedImageTokens = 1844

func estimateTokens(bytes int) int {
	return (bytes + 3) / 4
}

func messageTokens(m Message) int {
	size := messageBytes(m)
	images := 0
	for _, c := range m.Content {
		if c.File != nil && c.File.Data != "" {
			size -= len(c.File.Data)
			images++
		}
		if c.ToolCall != nil {
			size += len(c.ToolCall.Name) + len(c.ToolCall.ID)
		}
		if c.ToolResult != nil {
			size += len(c.ToolResult.ID)
		}
	}
	// Allow a little space for role and content framing as well as the payload.
	return 4 + estimateTokens(size) + images*estimatedImageTokens
}

func messagesTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += messageTokens(m)
	}
	return total
}

// A usage anchor covers measured input and output from a successful response.
// Only messages added after that boundary are estimated. If a provider omits
// output usage, estimate its assistant output too. Cached tokens occupy context.
type contextUsageAnchor struct {
	prefix   [32]byte
	revision uint64
	messages int
	tokens   int64
}

func requestPrefix(req *request) ([]byte, int) {
	tools, _ := json.Marshal(toTools(req.tools))
	schema, _ := json.Marshal(req.outputSchema)
	prefix, _ := json.Marshal([]string{req.model, req.effort, req.instructions, string(tools), string(schema)})
	return prefix, estimateTokens(len(req.instructions) + len(tools) + len(schema))
}

func (a *Agent) requestTokens(req *request) int64 {
	prefix, fixedTokens := requestPrefix(req)
	a.stateMu.RLock()
	anchor, revision := a.contextUsage, a.ContextRevision
	a.stateMu.RUnlock()
	if anchor.tokens > 0 && anchor.revision == revision && anchor.prefix == sha256.Sum256(prefix) && anchor.messages <= len(req.messages) {
		return anchor.tokens + int64(messagesTokens(req.messages[anchor.messages:]))
	}
	return int64(fixedTokens + messagesTokens(req.messages))
}

func (a *Agent) anchorContextUsage(req *request, resp *response) {
	usage := resp.usage
	if usage.InputTokens <= 0 {
		return
	}
	prefix, _ := requestPrefix(req)
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.contextUsage = contextUsageAnchor{
		prefix: sha256.Sum256(prefix), revision: a.ContextRevision,
		messages: len(req.messages), tokens: usage.InputTokens,
	}
	if usage.OutputTokens > 0 {
		a.contextUsage.messages += len(resp.messages)
		a.contextUsage.tokens += usage.OutputTokens
	}
}

func messageBytes(m Message) int {
	total := 0
	for _, c := range m.Content {
		total += len(c.Text) + len(c.Refusal)
		if c.File != nil {
			total += len(c.File.Data)
		}
		if c.ToolCall != nil {
			total += len(c.ToolCall.Args)
		}
		if c.ToolResult != nil {
			total += len(c.ToolResult.Content)
		}
		if c.Reasoning != nil {
			total += len(c.Reasoning.Summary) + len(c.Reasoning.Content)
		}
	}
	return total
}

// compactionOvershoot compares the upcoming input with the available budget.
func (a *Agent) compactionOvershoot(model string, inputTokens int64) int64 {
	if inputTokens <= 0 || a.Config.ContextWindow < 0 {
		return 0
	}
	return inputTokens - int64(a.contextInputBudget(model))
}

func (a *Agent) contextInputBudget(model string) int {
	window := a.Config.ContextWindow
	if window <= 0 {
		window = ContextWindowFor(model)
	}

	reserve := a.Config.ReserveTokens
	if reserve <= 0 {
		reserve = DefaultReserveTokens
		// A fixed default reserve is too thin a margin on large (1M) windows —
		// it would defer compaction to ~97% of the window and lean on the
		// reactive overflow path. Keep at least a 10% headroom, matching the
		// ~90% trigger other Responses-API agents use. An explicit
		// Config.ReserveTokens is honored as-is.
		if frac := window / 10; frac > reserve {
			reserve = frac
		}
		reserve = min(reserve, window/2)
	}

	return max(1, window-reserve)
}
