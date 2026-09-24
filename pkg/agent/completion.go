package agent

import (
	"fmt"
	"strings"
)

const maxAutomaticContinuations = 2

var errMissingFinalAnswer = fmt.Errorf("%w: the model did not send a final answer after two continuations. Send Continue to resume", ErrTurnIncomplete)

var errRepeatedCutoff = fmt.Errorf("%w: the response was cut off again before completion. Send Continue to resume", ErrTurnIncomplete)

var errRepeatedPause = fmt.Errorf("%w: the provider kept pausing after two continuations. Send Continue to resume", ErrTurnIncomplete)

const continuationReminder = `Your turn is still active. Continue the unfinished task using tools as needed, then give your final answer. A progress update does not finish the task.`

// completionState tracks recovery within one user turn. New accepted input,
// actual tool work, and Stop-hook continuations reset its recovery budget.
type completionState struct {
	answer   string
	attempts int
	cutoff   bool
}

func (s *completionState) advance(resp *response, requireFinish, finished, hasWork bool) (bool, *Message, error) {
	if hasWork {
		*s = completionState{}
		return true, nil, nil
	}

	// Empty cutoffs preserve an earlier completed answer. Replacement text,
	// even partial text, requires a fresh completed answer before finishing.
	text := lastAssistantText(resp.messages)
	if strings.TrimSpace(text) != "" {
		s.answer = ""
		if !resp.incomplete && resp.stopReason != "pause_turn" {
			s.answer = text
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

	var reason error
	switch {
	case resp.stopReason == "pause_turn":
		reason = errRepeatedPause
	case finished || hasRefusal(resp.messages):
	case requireFinish:
		reason = ErrMissingFinish
	case endsWithCommentary(resp.messages) || recovering && strings.TrimSpace(text) == "":
		reason = errMissingFinalAnswer
	}
	if reason == nil {
		s.attempts = 0
		return false, nil, nil
	}
	s.attempts++
	if s.attempts > maxAutomaticContinuations {
		return false, nil, reason
	}
	if resp.stopReason == "pause_turn" {
		// Resume native pauses with the preserved history, without inventing
		// a new user instruction or treating the pause as a missing marker.
		return true, nil, nil
	}
	reminder := continuationReminder
	if requireFinish {
		reminder = finishReminder
		if s.answer == "" {
			reminder = missingAnswerReminder
		}
	}
	notice := hiddenContextMessage(reminder)
	return true, &notice, nil
}
