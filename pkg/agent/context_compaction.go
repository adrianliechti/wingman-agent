package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/text"
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

	summary, err := a.summarizeContext(ctx, req, messages)
	if err != nil {
		return fmt.Errorf("compact context: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if summary == "" {
		return fmt.Errorf("compaction returned an empty summary; conversation preserved")
	}
	checkpoint := hiddenContextMessage(summaryPrefix + "\n\n" + summary + a.sessionFacts() + "\n\n" + summaryContinuation)
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
