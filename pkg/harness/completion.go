package harness

import (
	"fmt"
	"strings"
)

const maxAutomaticContinuations = 2

var errMissingFinalAnswer = fmt.Errorf("%w: the model did not send a final answer after two continuations. Send Continue to resume", ErrTurnIncomplete)

var errRepeatedCutoff = fmt.Errorf("%w: the response was cut off again before completion. Send Continue to resume", ErrTurnIncomplete)

var errRepeatedPause = fmt.Errorf("%w: the provider kept pausing after two continuations. Send Continue to resume", ErrTurnIncomplete)

const continuationReminder = `Your turn is still active. Continue the unfinished task using tools as needed, then give your final answer. A progress update does not finish the task. Do not repeat completed tool work.`

// completionState tracks recovery within one user turn. New accepted input,
// actual tool work, and Stop-hook continuations reset its recovery budget.
type completionState struct {
	answer             assistantText
	attempts           int
	cutoff             bool
	confirming         bool
	nativeContinuation bool
}

func (s *completionState) advance(resp *response, mode finishMode, finish finishStatus, hasWork bool) (bool, *Message, error) {
	if hasWork {
		*s = completionState{}
		return true, nil, nil
	}

	// Empty cutoffs preserve an earlier completed answer. Replacement text,
	// even partial text, requires a fresh completed answer before finishing.
	// Rejecting a marker does not invalidate completed user-facing text.
	last := lastAssistantTextItem(resp.messages)
	hasText := strings.TrimSpace(last.text) != ""
	if hasText {
		s.answer = assistantText{}
		if !resp.incomplete && resp.stopReason != "pause_turn" && !last.isProgress() {
			s.answer = last
		}
	}
	if resp.incomplete {
		if s.cutoff {
			return false, nil, errRepeatedCutoff
		}
		s.cutoff = true
		notice := cutoffNotice(resp.incompleteReason)
		return true, &notice, nil
	}
	recovering := s.cutoff || s.attempts > 0
	s.cutoff = false
	switch {
	case finish == finishAccepted || resp.stopReason == "refusal" || resp.refused:
		return false, nil, nil
	case resp.stopReason == "pause_turn":
		return s.retry(errRepeatedPause, "")
	case resp.endTurn != nil && finish != finishRejected:
		s.attempts = 0
		s.confirming = false
		return !*resp.endTurn, nil, nil
	case mode == finishAutomatic && last.phase == "",
		mode == finishConfirming && (last.phase != PhaseFinalAnswer || finish == finishRejected):
		s.confirming = true
		// The next request advertises finish_turn. Keep the history valid
		// when the model resumes tool work and the marker goes away.
		return s.retry(ErrMissingFinish, continuationReminder)
	case finish == finishRejected || last.isProgress() || recovering && !hasText:
		return s.retry(errMissingFinalAnswer, continuationReminder)
	}
	return false, nil, nil
}

// A native pause resumes with the existing history. Other recovery paths
// supply a reminder but use the same bounded number of additional requests.
func (s *completionState) retry(exhausted error, reminder string) (bool, *Message, error) {
	if s.attempts >= maxAutomaticContinuations {
		return false, nil, exhausted
	}
	s.attempts++
	if reminder == "" {
		return true, nil, nil
	}
	notice := hiddenContextMessage(reminder)
	return true, &notice, nil
}
