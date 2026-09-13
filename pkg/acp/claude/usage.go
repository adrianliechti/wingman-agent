package claude

import (
	"encoding/json"

	"github.com/coder/acp-go-sdk"
)

// Accessed only by the process reader.
type contextUsage struct {
	model       string
	used        *int
	window      int
	streamUsage *cliUsage
}

func (u cliUsage) totalTokens() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

func (c *contextUsage) setModel(model string) {
	model = realModelID(model)
	if model == "" {
		return
	}
	if canonicalModelID(model) != canonicalModelID(c.model) {
		c.window = 0
		c.used = nil
		c.streamUsage = nil
	}
	c.model = model
}

func (c *contextUsage) observeAssistant(message cliMessage) {
	if message.Model != "" && realModelID(message.Model) == "" {
		c.streamUsage = nil
		return
	}
	c.setModel(message.Model)
	c.streamUsage = message.Usage
	c.used = nil
	if message.Usage != nil {
		c.used = new(message.Usage.totalTokens())
	}
}

func (c *contextUsage) observeStream(event streamEvent) {
	switch event.Type {
	case "message_start":
		c.observeAssistant(event.Message)
	case "message_delta":
		if c.streamUsage != nil && len(event.Usage) > 0 {
			// Usage deltas may omit the input/cache fields from message_start.
			usage := *c.streamUsage
			if json.Unmarshal(event.Usage, &usage) == nil {
				c.streamUsage = &usage
				c.used = new(usage.totalTokens())
			}
		}
	}
}

func (c *contextUsage) compact(postTokens *int) {
	c.streamUsage = nil
	c.used = nil
	if postTokens != nil {
		c.used = new(max(0, *postTokens))
	}
}

func (c *contextUsage) resultUpdate(result cliResult, models []ModelEntry) *acp.SessionUpdate {
	ids := []string{c.model}
	if model := resolveModel(models, c.model); model != nil && model.ResolvedModel != "" {
		ids = append(ids, model.ResolvedModel)
	}
	for _, id := range ids {
		for model, usage := range result.ModelUsage {
			if canonicalModelID(model) == canonicalModelID(id) && usage.ContextWindow > 0 {
				c.window = usage.ContextWindow
				return c.update(result.TotalCostUSD)
			}
		}
	}
	if (c.model == "" || c.model == "default") && len(result.ModelUsage) == 1 {
		for _, usage := range result.ModelUsage {
			c.window = usage.ContextWindow
		}
	}
	return c.update(result.TotalCostUSD)
}

func (c *contextUsage) update(cost float64) *acp.SessionUpdate {
	if c.used == nil || c.window <= 0 {
		return nil
	}
	update := &acp.SessionUsageUpdate{SessionUpdate: "usage_update", Used: max(0, *c.used), Size: c.window}
	if cost > 0 {
		update.Cost = &acp.Cost{Amount: cost, Currency: "USD"}
	}
	return &acp.SessionUpdate{UsageUpdate: update}
}
