package code

import (
	"context"
	"errors"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
)

type TurnInputIntent string

const (
	TurnInputFollowUp TurnInputIntent = "follow_up"
	TurnInputSteer    TurnInputIntent = "steer"
)

type TurnInputState string

const (
	TurnInputQueued    TurnInputState = "queued"
	TurnInputActive    TurnInputState = "active"
	TurnInputSteered   TurnInputState = "steered"
	TurnInputCompleted TurnInputState = "completed"
	TurnInputCancelled TurnInputState = "cancelled"
	TurnInputFailed    TurnInputState = "failed"
)

type TurnFeatures struct {
	Steer bool `json:"steer"`
}

// TurnFeatureProvider advertises optional in-turn behavior. FIFO follow-up
// queueing is supplied by TurnManager for every Agent; providers only need to
// advertise behavior that must be implemented by the backend itself.
type TurnFeatureProvider interface {
	TurnFeatures(sessionID string) TurnFeatures
}

// TurnSteerer injects input into the currently active turn. ErrNoActiveTurn and
// ErrTurnNotSteerable ask TurnManager to preserve the input as a FIFO follow-up;
// other errors are returned to the caller.
type TurnSteerer interface {
	Steer(ctx context.Context, sessionID string, input TurnInput) error
}

var (
	// ErrNoActiveTurn means a steer lost the active-turn boundary race.
	ErrNoActiveTurn = errors.New("no active turn")
	// ErrTurnNotSteerable means the active backend turn rejects same-turn input.
	ErrTurnNotSteerable = errors.New("active turn is not steerable")
	// ErrInputNotQueued means a queue mutation targeted a non-queued input.
	ErrInputNotQueued = errors.New("turn input is not queued")
	// ErrDuplicateInput means an input ID is already live in the session.
	ErrDuplicateInput = errors.New("turn input id already exists")
)

type TurnInput struct {
	ID      string
	Content []agent.Content
	Intent  TurnInputIntent
}

type TurnInputSnapshot struct {
	ID       string
	State    TurnInputState
	Intent   TurnInputIntent
	Position int
}

type TurnSnapshot struct {
	Inputs   []TurnInputSnapshot
	Paused   bool
	Features TurnFeatures
}

type TurnEvent struct {
	SessionID string
	InputID   string
	State     TurnInputState
	Intent    TurnInputIntent
	Position  int
	Message   *agent.Message
	Err       error
	// Executed is true only for the primary input whose Agent.Send call ended.
	// Steered and removed queued inputs also receive terminal states but must not
	// trigger turn-finalization side effects such as checkpoints.
	Executed bool
}

func cloneContent(in []agent.Content) []agent.Content {
	out := make([]agent.Content, len(in))
	for i, c := range in {
		out[i] = c
		if c.File != nil {
			file := *c.File
			out[i].File = &file
		}
		if c.Reasoning != nil {
			reasoning := *c.Reasoning
			out[i].Reasoning = &reasoning
		}
		if c.ToolCall != nil {
			call := *c.ToolCall
			out[i].ToolCall = &call
		}
		if c.ToolResult != nil {
			result := *c.ToolResult
			out[i].ToolResult = &result
		}
	}
	return out
}
