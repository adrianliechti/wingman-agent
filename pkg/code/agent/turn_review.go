package agent

import (
	"context"
	"fmt"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
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
	return s.aa.UndoTurn(ctx, turnID, func(ctx context.Context, files []tool.FileChange) error {
		return fs.UndoFileChanges(ctx, a.workspace.Root, files)
	})
}
