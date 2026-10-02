package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/text"
)

const maxRetainedUserTokens = 20_000
const maxRetainedWindowTokens = 20_000
const summaryOutputTokens = 8_000
const summaryPrefix = "[Previous conversation summary]"
const summaryContinuation = "Continue the active task from this checkpoint. Earlier requests remain the task scope; later messages may steer it. Do not repeat completed work or treat compaction as a new user request."
const windowContinuation = " The messages after this checkpoint are the most recent part of the conversation, kept verbatim; they continue where the summary ends."

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

// retainedWindowStart returns where the verbatim recent window begins: the
// earliest step boundary such that messages[start:] fits the budget. A window
// never splits a tool batch from its results and never reaches back past an
// earlier checkpoint. Session-context snapshots are budgeted separately.
// len(messages) means no window.
func retainedWindowStart(messages []Message, budget int) int {
	start := len(messages)
	total := 0
	pendingCalls := make(map[string]bool)
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if isCompactionSummary(m) {
			break
		}
		if isSessionContext(m) {
			continue
		}
		total += messageTokens(m)
		if total > budget {
			break
		}
		// A response can interleave text or reasoning with separate tool-call
		// messages. Walking backwards, every result must reach its call before
		// such a message is a safe boundary. Process results first so a message
		// containing both sides of a pair also balances correctly.
		for _, c := range m.Content {
			if c.ToolResult != nil {
				pendingCalls[c.ToolResult.ID] = true
			}
		}
		for _, c := range m.Content {
			if c.ToolCall != nil {
				delete(pendingCalls, c.ToolCall.ID)
			}
		}
		if len(pendingCalls) == 0 && isStepBoundary(m) {
			start = i
		}
	}
	return start
}

// User input and assistant text or reasoning are candidate step boundaries.
// retainedWindowStart also checks that no tool pair crosses the boundary.
func isStepBoundary(m Message) bool {
	if m.Role != RoleUser && m.Role != RoleAssistant {
		return false
	}
	for _, c := range m.Content {
		if c.ToolCall != nil || c.ToolResult != nil {
			return false
		}
	}
	return true
}

// recentWindow copies the verbatim tail without superseded session-context
// snapshots. The latest snapshot keeps its place when it is inside the tail.
func recentWindow(tail []Message, latestSnapshot int) []Message {
	window := make([]Message, 0, len(tail))
	for i, m := range tail {
		if isSessionContext(m) && i != latestSnapshot {
			continue
		}
		window = append(window, m)
	}
	return window
}

func (a *Agent) checkpointMessage(summary string, window bool) Message {
	continuation := summaryContinuation
	if window {
		continuation += windowContinuation
	}
	return hiddenContextMessage(summaryPrefix + "\n\n" + summary + a.sessionFacts() + "\n\n" + continuation)
}

// Rebuild from bounded user input, a single continuation summary, and a
// verbatim window of the most recent steps, so the model keeps the evidence it
// was just working from instead of re-fetching it. Layout: older user messages
// (whole, newest-first within budget), current guidance, the checkpoint, then
// the recent window. Without a window the checkpoint stays last and guidance
// precedes the last user input, like Codex's local compaction. Original
// messages remain in the canonical ledger.
func (a *Agent) compactMessages(ctx context.Context, req *request) error {
	messages := a.requestMessages()
	if len(messages) == 0 {
		return fmt.Errorf("context limit reached with no history to compact; conversation preserved")
	}
	_, fixed := requestPrefix(req)
	available := max(0, a.contextInputBudget(req.model)-fixed)
	guidanceIndex := lastSessionContextIndex(messages)
	var guidance *Message
	if guidanceIndex >= 0 {
		guidance = &messages[guidanceIndex]
		available -= messageTokens(*guidance)
	}

	// Leave most of the budget for new work. The recent window is optional:
	// if it prevents compaction, retry once with the full history as summary
	// input. Never drop a window using a summary that has not seen its evidence.
	start := retainedWindowStart(messages, min(maxRetainedWindowTokens, available/3))
	for {
		head := messages[:start]
		window := recentWindow(messages[start:], guidanceIndex-start)
		guidanceInWindow := guidanceIndex >= start
		remaining := available
		for _, m := range window {
			if !isSessionContext(m) {
				remaining -= messageTokens(m)
			}
		}
		// A latest input inside the window is already verbatim.
		guaranteeLatest := lastVisibleUserIndex(messages) < start

		retained, err := compactionUserMessages(head, remaining, guaranteeLatest)
		kept := len(retained) + len(window)
		if guidance != nil && !guidanceInWindow {
			kept++
		}
		if err == nil && kept == len(messages) {
			// This includes histories with leading session snapshots, which
			// are skipped when selecting the window. Measured usage can still
			// require compaction even when the entire estimated history fits.
			err = fmt.Errorf("context limit reached with no history to compact; conversation preserved")
		}
		if err != nil {
			if start < len(messages) {
				start = len(messages)
				continue
			}
			return err
		}

		summary, err := a.summarizeContext(ctx, req, head)
		if err != nil {
			return fmt.Errorf("compact context: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if summary == "" {
			return fmt.Errorf("compaction returned an empty summary; conversation preserved")
		}
		checkpoint := a.checkpointMessage(summary, len(window) > 0)
		retained, err = compactionUserMessages(head, remaining-messageTokens(checkpoint), guaranteeLatest)
		if err != nil {
			if start < len(messages) {
				start = len(messages)
				continue
			}
			return err
		}
		if guidance != nil && !guidanceInWindow {
			at := len(retained)
			if len(window) == 0 {
				at = max(0, len(retained)-1)
			}
			retained = slices.Insert(retained, at, *guidance)
		}
		compacted := append(retained, checkpoint)
		compacted = append(compacted, window...)
		if messagesTokens(compacted) >= messagesTokens(messages) {
			if start < len(messages) {
				start = len(messages)
				continue
			}
			return fmt.Errorf("compaction did not reduce context; conversation preserved")
		}
		return a.replaceContext("compact model context", compacted)
	}
}

const (
	maxFactFiles        = 40
	maxFactChecks       = 10
	maxFactCommandBytes = 160
)

// The ledger records edits and checks exactly. Restating them keeps the
// checkpoint from depending on the summary for facts it can prove.
func (a *Agent) sessionFacts() string {
	type checkKey struct{ command, workdir string }
	var files []string
	var order []checkKey
	checks := map[checkKey]ValidationCheck{}
	for _, review := range a.TurnReviews() {
		if review.Undone {
			continue
		}
		if len(review.Files) > 0 {
			for key, check := range checks {
				if check.Outcome == "passed" {
					check.Outcome = "outdated"
					checks[key] = check
				}
			}
		}
		for _, change := range review.Files {
			if !slices.Contains(files, change.Path) {
				files = append(files, change.Path)
			}
		}
		for _, check := range review.Checks {
			key := checkKey{check.Command, check.WorkDir}
			order = slices.DeleteFunc(order, func(k checkKey) bool { return k == key })
			order = append(order, key)
			checks[key] = check
		}
	}
	if len(files) == 0 && len(order) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nRecorded by the harness:")
	if len(files) > 0 {
		b.WriteString("\n- Files changed in this session: " + strings.Join(files[:min(len(files), maxFactFiles)], ", "))
		if len(files) > maxFactFiles {
			fmt.Fprintf(&b, " (and %d more)", len(files)-maxFactFiles)
		}
	}
	for _, key := range order[max(0, len(order)-maxFactChecks):] {
		check := checks[key]
		command := text.TruncateHead(strings.Join(strings.Fields(check.Command), " "), maxFactCommandBytes)
		fmt.Fprintf(&b, "\n- Check `%s`: %s", command, checkOutcome(check))
	}
	return b.String()
}

func checkOutcome(check ValidationCheck) string {
	outcome := "result not confirmed"
	switch check.Outcome {
	case "passed":
		outcome = "passed"
	case "failed":
		outcome = "failed"
	case "outdated":
		outcome = "passed before later edits"
	}
	if check.ExitCode != nil {
		outcome += fmt.Sprintf(" (exit %d)", *check.ExitCode)
	}
	return outcome
}

// compactionUserMessages selects the user messages to keep verbatim. With
// guaranteeLatest, the newest visible user message must fit or compaction
// fails; the recent window makes that unnecessary when it holds the message.
func compactionUserMessages(messages []Message, available int, guaranteeLatest bool) ([]Message, error) {
	if available < 0 {
		return nil, fmt.Errorf("compaction summary or session context exceeds the available context budget; conversation preserved")
	}
	budget := min(maxRetainedUserTokens, max(0, available))
	if idx := lastVisibleUserIndex(messages); idx >= 0 && guaranteeLatest {
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
