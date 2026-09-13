package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

const maxRetainedUserTokens = 20_000
const summaryOutputTokens = 8_000
const summaryPrefix = "[Previous conversation summary]"
const summaryContinuation = "Continue the active task from this checkpoint. Earlier requests remain the task scope; later messages may steer it. Do not repeat completed work or treat compaction as a new user request."

func isCompactionSummary(m Message) bool {
	return m.Hidden && m.Role == RoleUser && strings.HasPrefix(contentText(m.Content), summaryPrefix+"\n")
}

// Select whole user messages newest-first and replay in chronological order.
// The first message that does not fit stays in the summary source, rather than
// silently presenting a cut-off instruction as the user's original request.
func retainedUserMessages(messages []Message, budget int) []Message {
	var retained []Message
	for _, m := range slices.Backward(messages) {
		if m.Role != RoleUser || m.Hidden {
			continue
		}
		cost := messageTokens(m)
		if cost > budget {
			break
		}
		retained = append(retained, m)
		budget -= cost
	}
	slices.Reverse(retained)
	return retained
}

// Like Codex's local compaction, rebuild from bounded user input and a single
// continuation summary. Keep current guidance before the last user input and
// the checkpoint last. Original messages remain in the canonical ledger.
func (a *Agent) compactMessages(ctx context.Context, req *request) error {
	messages := a.requestMessages()
	if len(messages) == 0 {
		return fmt.Errorf("context limit reached with no history to compact; conversation preserved")
	}
	_, fixed := requestPrefix(req)
	available := max(0, a.contextInputBudget(req.model)-fixed)
	var guidance *Message
	if i := lastSessionContextIndex(messages); i >= 0 {
		guidance = &messages[i]
		available -= messageTokens(*guidance)
	}
	retained, err := compactionUserMessages(messages, available)
	if err != nil {
		return err
	}
	kept := len(retained)
	if guidance != nil {
		kept++
	}
	if kept == len(messages) {
		return fmt.Errorf("context limit reached with no history to compact; conversation preserved")
	}

	// Completion usage includes hidden reasoning on some models. Reserve a
	// generation budget independently, then budget the actual briefing text.
	summary, err := a.generateBriefing(ctx, compactionInstructions, messages, summaryOutputTokens)
	if err != nil {
		return fmt.Errorf("compact context: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if summary == "" {
		return fmt.Errorf("compaction returned an empty summary; conversation preserved")
	}
	checkpoint := hiddenContextMessage(summaryPrefix + "\n\n" + summary + "\n\n" + summaryContinuation)
	retained, err = compactionUserMessages(messages, available-messageTokens(checkpoint))
	if err != nil {
		return err
	}
	if guidance != nil {
		retained = slices.Insert(retained, max(0, len(retained)-1), *guidance)
	}
	compacted := append(retained, checkpoint)
	if messagesTokens(compacted) >= messagesTokens(messages) {
		return fmt.Errorf("compaction did not reduce context; conversation preserved")
	}
	return a.replaceContext("compact model context", compacted)
}

func compactionUserMessages(messages []Message, available int) ([]Message, error) {
	if available < 0 {
		return nil, fmt.Errorf("compaction summary or session context exceeds the available context budget; conversation preserved")
	}
	budget := min(maxRetainedUserTokens, max(0, available))
	if idx := lastVisibleUserIndex(messages); idx >= 0 {
		latest := messageTokens(messages[idx])
		if latest > available {
			return nil, fmt.Errorf("cannot compact without shortening the latest user input (estimated %d tokens, %d available); conversation preserved", latest, max(0, available))
		}
		// An oversized latest input may use the remaining capacity, verbatim.
		budget = max(budget, latest)
	}
	return retainedUserMessages(messages, budget), nil
}

func lastVisibleUserIndex(messages []Message) int {
	for i, message := range slices.Backward(messages) {
		if message.Role == RoleUser && !message.Hidden {
			return i
		}
	}
	return -1
}
