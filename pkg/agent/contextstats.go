package agent

import (
	"encoding/json"
	"sort"
)

type ToolStat struct {
	Name   string
	Tokens int
}

// ContextStats estimates what occupies the model's context window, by
// category. Text uses ~4 bytes/token and images use a fixed token estimate;
// LastInputTokens is the provider-reported figure for the latest request.
type ContextStats struct {
	Model  string
	Window int

	InstructionsTokens int
	ToolsTokens        int
	ToolStats          []ToolStat
	MessagesTokens     int
	MessageCount       int

	LastInputTokens int64
	// CurrentTokens combines the latest applicable response usage with an
	// estimate of subsequent additions; rewrites and model changes invalidate it.
	CurrentTokens int64
}

func (s ContextStats) EstimatedTotal() int {
	return s.InstructionsTokens + s.ToolsTokens + s.MessagesTokens
}

func (a *Agent) ContextStats() ContextStats {
	stats := ContextStats{}
	req := &request{messages: a.requestMessages()}

	if a.Config != nil {
		if a.Model != nil {
			stats.Model = a.Model()
		}
		req.model = stats.Model
		if a.Effort != nil {
			req.effort = a.Effort()
		}

		stats.Window = a.ContextWindow
		if stats.Window <= 0 {
			stats.Window = ContextWindowFor(stats.Model)
		}

		if a.Instructions != nil {
			req.instructions = a.Instructions()
			stats.InstructionsTokens = estimateTokens(len(req.instructions))
		}
		if update, changed := a.contextInstructionsUpdate(req.messages); changed {
			req.messages = append(req.messages, update)
		}

		if a.Tools != nil {
			req.tools = a.Tools()
			for _, t := range req.tools {
				size := len(t.Name) + len(t.Description)
				if params, err := json.Marshal(t.Parameters); err == nil {
					size += len(params)
				}
				tokens := estimateTokens(size)
				stats.ToolsTokens += tokens
				stats.ToolStats = append(stats.ToolStats, ToolStat{Name: t.Name, Tokens: tokens})
			}
			sort.Slice(stats.ToolStats, func(i, j int) bool {
				return stats.ToolStats[i].Tokens > stats.ToolStats[j].Tokens
			})
		}
	}

	for _, m := range req.messages {
		stats.MessageCount++
		stats.MessagesTokens += messageTokens(m)
	}

	stats.LastInputTokens = a.UsageSnapshot().LastInputTokens
	stats.CurrentTokens = a.requestTokens(req)

	return stats
}
