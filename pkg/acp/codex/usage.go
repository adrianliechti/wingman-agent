package codex

import "github.com/coder/acp-go-sdk"

// tokenBreakdown mirrors Codex's TokenUsageBreakdown. inputTokens is the
// whole prompt, including cache reads and cache writes.
type tokenBreakdown struct {
	TotalTokens           int `json:"totalTokens"`
	InputTokens           int `json:"inputTokens"`
	CachedInputTokens     int `json:"cachedInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
	OutputTokens          int `json:"outputTokens"`
	ReasoningOutputTokens int `json:"reasoningOutputTokens"`
}

type tokenUsageUpdate struct {
	TokenUsage struct {
		Total              *tokenBreakdown `json:"total"`
		Last               tokenBreakdown  `json:"last"`
		ModelContextWindow int             `json:"modelContextWindow"`
	} `json:"tokenUsage"`
}

// Keep thread totals on the RPC reader, including restored history and updates
// delivered after cancellation stops the prompt's notification consumer.
func (c *codexClient) setThreadTokenTotal(threadID string, total tokenBreakdown) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenTotals[threadID] = total
}

func (c *codexClient) threadTokenTotal(threadID string) *tokenBreakdown {
	c.mu.Lock()
	defer c.mu.Unlock()
	total, ok := c.tokenTotals[threadID]
	if !ok {
		return nil
	}
	return &total
}

func (a tokenBreakdown) combine(b tokenBreakdown, op func(x, y int) int) tokenBreakdown {
	return tokenBreakdown{
		TotalTokens:           op(a.TotalTokens, b.TotalTokens),
		InputTokens:           op(a.InputTokens, b.InputTokens),
		CachedInputTokens:     op(a.CachedInputTokens, b.CachedInputTokens),
		CacheWriteInputTokens: op(a.CacheWriteInputTokens, b.CacheWriteInputTokens),
		OutputTokens:          op(a.OutputTokens, b.OutputTokens),
		ReasoningOutputTokens: op(a.ReasoningOutputTokens, b.ReasoningOutputTokens),
	}
}

func (a tokenBreakdown) below(b tokenBreakdown) bool {
	return a.TotalTokens < b.TotalTokens || a.InputTokens < b.InputTokens ||
		a.CachedInputTokens < b.CachedInputTokens || a.CacheWriteInputTokens < b.CacheWriteInputTokens ||
		a.OutputTokens < b.OutputTokens || a.ReasoningOutputTokens < b.ReasoningOutputTokens
}

// acpUsage separates fresh input, cache reads, and cache writes, and reports
// reasoning apart from visible output, clamping inconsistent provider data.
func (a tokenBreakdown) acpUsage() *acp.Usage {
	cachedRead := max(0, a.CachedInputTokens)
	cachedWrite := max(0, a.CacheWriteInputTokens)
	output := max(0, a.OutputTokens)
	reasoning := min(max(0, a.ReasoningOutputTokens), output)
	return &acp.Usage{
		TotalTokens:       a.TotalTokens,
		InputTokens:       max(0, a.InputTokens-cachedRead-cachedWrite),
		OutputTokens:      output - reasoning,
		CachedReadTokens:  &cachedRead,
		CachedWriteTokens: &cachedWrite,
		ThoughtTokens:     &reasoning,
	}
}

func sub(x, y int) int { return x - y }
func add(x, y int) int { return x + y }

// promptTokenUsage accumulates one prompt's usage from Codex's thread token
// updates. Each update carries the thread total and the latest model request;
// a prompt that makes several requests must report all of them. The usage is
// the thread total at the end minus the total at the start, or the sum of the
// distinct latest-request counts when the start is unknown or the total moved
// backwards (for example after compaction).
type promptTokenUsage struct {
	// Owned by the prompt's event loop; the RPC reader tracks thread totals
	// separately on codexClient.
	start, previous *tokenBreakdown
	summed          tokenBreakdown
	reported        bool
	useSum          bool
	modelWork       bool
}

// newPromptTokenUsage starts from the thread total before the prompt, or nil
// when the thread has history whose total Codex has not reported yet.
func newPromptTokenUsage(start *tokenBreakdown) *promptTokenUsage {
	u := &promptTokenUsage{}
	if start != nil {
		s := *start
		u.start, u.previous = &s, &s
	}
	return u
}

// observeModelWork records model output or the start of a compaction request,
// so a later update belongs to this prompt's model work.
func (u *promptTokenUsage) observeModelWork() { u.modelWork = true }

// Restored history or interrupt cleanup can arrive after the prompt captured
// its baseline, tagged with an older turn. Refresh only before new model work.
func (u *promptTokenUsage) observeHistory(total tokenBreakdown) {
	if !u.modelWork {
		u.start, u.previous = &total, &total
	}
}

func (u *promptTokenUsage) observe(total, last tokenBreakdown) {
	if u.previous != nil && *u.previous == total {
		return
	}
	if !u.modelWork {
		// Reports before model work describe earlier history. They can also
		// refresh a baseline after interrupt cleanup.
		u.start, u.previous = &total, &total
		return
	}
	// Codex fills a context-overflow report to the model's window and also
	// emits context estimates after compaction. A positive total without any
	// input/output components describes context occupancy, not model spend.
	// Retain it for deduplication and invalidate the cumulative subtraction,
	// but keep the real requests already accumulated for this prompt.
	if last.TotalTokens > 0 && last == (tokenBreakdown{TotalTokens: last.TotalTokens}) {
		u.previous = &total
		u.useSum = true
		return
	}
	if u.previous != nil && total.below(*u.previous) {
		u.useSum = true
	}
	u.summed = u.summed.combine(last, add)
	u.previous, u.reported = &total, true
}

// usage reports the prompt's usage so far, or nil when Codex reported none.
func (u *promptTokenUsage) usage() *acp.Usage {
	if !u.reported {
		return nil
	}
	if u.start != nil && !u.useSum {
		return u.previous.combine(*u.start, sub).acpUsage()
	}
	return u.summed.acpUsage()
}
