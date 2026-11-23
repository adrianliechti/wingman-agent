package harness

import (
	"context"
	"fmt"
	"strings"
)

const maxAutomaticContinuations = 2

var errMissingFinalAnswer = fmt.Errorf("%w: the model did not send a final answer after two continuations. Send Continue to resume", ErrTurnIncomplete)

var errRepeatedCutoff = fmt.Errorf("%w: the response was cut off again before completion. Send Continue to resume", ErrTurnIncomplete)

var errRepeatedPause = fmt.Errorf("%w: the provider kept pausing after two continuations. Send Continue to resume", ErrTurnIncomplete)

const continuationReminder = `Your turn is still active. Continue the unfinished task using tools as needed, then give your final answer. A progress update does not finish the task. Do not repeat completed tool work.`

// ErrMissingFinish marks an unphased turn that exhausted automatic recovery.
var ErrMissingFinish = fmt.Errorf("%w: the model still has unfinished work after two continuations. Send Continue to resume", ErrTurnIncomplete)

type completionState struct {
	attempts           int
	unphasedRetries    int
	cutoff             bool
	nativeContinuation bool
}

func (s *completionState) afterToolWork() {
	s.attempts = 0
	s.cutoff = false
}

func (s *completionState) advance(ctx context.Context, a *Agent, req *request, resp *response, hasWork bool) (bool, *Message, error) {
	if hasWork {
		s.afterToolWork()
		return true, nil, nil
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
	last := lastAssistantTextItem(resp.messages)
	switch {
	case resp.stopReason == "refusal" || resp.refused:
		return false, nil, nil
	case resp.stopReason == "pause_turn":
		return s.retry(errRepeatedPause, "")
	case resp.endTurn != nil:
		s.attempts = 0
		return !*resp.endTurn, nil, nil
	case last.isProgress():
		return s.retry(errMissingFinalAnswer, continuationReminder)
	case a.ConfirmUnphased && len(req.tools) > 0 && last.phase == "":
		if strings.TrimSpace(last.text) != "" {
			if a.hasPendingSteering() {
				return true, nil, nil
			}
			progress, err := a.checkProgress(ctx, req.messages, last.text)
			if err != nil || !progress {
				return false, nil, err
			}
		}
		// Intervening tool work cannot turn a later progress-only stop into success.
		if s.unphasedRetries >= maxAutomaticContinuations {
			return false, nil, ErrMissingFinish
		}
		s.unphasedRetries++
		return s.retry(ErrMissingFinish, continuationReminder)
	case recovering && strings.TrimSpace(last.text) == "":
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
