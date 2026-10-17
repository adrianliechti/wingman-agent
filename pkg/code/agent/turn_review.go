package agent

import (
	"context"
	"fmt"

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
)

func (a *Agent) TurnReviews(ctx context.Context, id string) ([]harness.TurnReview, error) {
	if a.session(id) == nil {
		if err := a.LoadSession(ctx, id); err != nil {
			return nil, err
		}
	}
	s := a.session(id)
	if s == nil {
		return nil, fmt.Errorf("session not found")
	}
	return s.aa.TurnReviews(), nil
}

func (a *Agent) UndoTurn(ctx context.Context, sessionID, turnID string) error {
	s := a.session(sessionID)
	if s == nil {
		return fmt.Errorf("session not loaded")
	}
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.closed || s.aa.Running() {
		return fmt.Errorf("stop the active turn before undoing edits")
	}
	for _, review := range s.aa.TurnReviews() {
		if review.ID != turnID {
			continue
		}
		if review.Undone {
			return fmt.Errorf("these edits were already undone")
		}
		if review.Outcome == "running" || review.Uncertain {
			return fmt.Errorf("this turn has an unconfirmed outcome; inspect the files first")
		}
		if len(review.Files) == 0 {
			return fmt.Errorf("this turn has no tracked edits to undo")
		}
		if err := fs.UndoFileChanges(ctx, a.workspace.Root, review.Files); err != nil {
			return err
		}
		if err := s.aa.RecordTurnUndo(turnID); err != nil {
			return fmt.Errorf("edits were undone, but the undo record could not be saved: %w", err)
		}
		return nil
	}
	return fmt.Errorf("turn not found")
}
