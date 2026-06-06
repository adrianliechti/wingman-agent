package code

import (
	"context"
	"iter"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
)

type SessionInfo struct {
	ID        string
	Title     string
	UpdatedAt time.Time
}

type Mode struct {
	ID          string
	Name        string
	Description string
}

type Agent interface {
	Name() string

	Workspace() *Workspace

	Models(sessionID string) (available []Model, current string)

	SetModel(ctx context.Context, sessionID, id string) error

	Effort(sessionID string) (current string, options []string)

	SetEffort(ctx context.Context, sessionID, value string) error

	Modes(sessionID string) (available []Mode, current string)

	SetMode(ctx context.Context, sessionID, modeID string) error

	ListSessions(ctx context.Context) ([]SessionInfo, error)

	NewSession(ctx context.Context) (string, error)

	LoadSession(ctx context.Context, id string) error

	DeleteSession(ctx context.Context, id string) error

	Messages(sessionID string) []agent.Message

	Usage(sessionID string) agent.Usage

	Send(ctx context.Context, sessionID string, input []agent.Content) iter.Seq2[agent.Message, error]

	Cancel(sessionID string)

	Close() error
}

type SessionLoadStreamer interface {
	LoadSessionStream(ctx context.Context, id string) iter.Seq2[[]agent.Message, error]
}
