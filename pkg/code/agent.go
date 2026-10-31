package code

import (
	"context"
	"iter"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/model"
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

const (
	AgentModeID      = "agent"
	PlanModeID       = "plan"
	UnattendedModeID = "unattended"
)

func UnattendedMode() Mode {
	return Mode{
		ID:          UnattendedModeID,
		Name:        "Unattended",
		Description: "Run every action without review and continue without questions.",
	}
}

// UnattendedElicitation resolves questions without UI. Defaults win, followed
// by the first (recommended) option; required free-text questions are declined
// rather than answered with fabricated input.
func UnattendedElicitation(req tool.ElicitRequest) tool.ElicitResult {
	content := map[string]any{}
	for _, field := range req.Fields {
		if field.CustomAnswerFor != "" {
			continue
		}
		switch {
		case field.Default != nil:
			content[field.Name] = field.Default
		case len(field.Enum) > 0 && field.Multiple:
			content[field.Name] = []string{field.Enum[0]}
		case len(field.Enum) > 0:
			content[field.Name] = field.Enum[0]
		case field.Type == "boolean":
			content[field.Name] = true
		case field.Required:
			return tool.ElicitResult{Action: tool.ElicitDecline}
		}
	}
	return tool.ElicitResult{Action: tool.ElicitAccept, Content: content}
}

type Command struct {
	Name        string
	Description string
	InputHint   string
}

type Agent interface {
	Name() string

	Workspace() *Workspace

	Models(sessionID string) (available []model.Model, current string)

	SetModel(ctx context.Context, sessionID, id string) error

	Effort(sessionID string) (current string, options []string)

	SetEffort(ctx context.Context, sessionID, value string) error

	Modes(sessionID string) (available []Mode, current string)

	SetMode(ctx context.Context, sessionID, modeID string) error

	ListSessions(ctx context.Context) ([]SessionInfo, error)

	NewSession(ctx context.Context) (string, error)

	LoadSession(ctx context.Context, id string) error

	DeleteSession(ctx context.Context, id string) error

	Messages(sessionID string) []harness.Message

	Usage(sessionID string) harness.Usage

	// Send starts one turn and returns its event stream. Immediate validation
	// and busy-session failures are returned directly; errors that happen after
	// the turn starts are yielded by the stream. Send never queues implicitly.
	// Once Send returns successfully, the agent owns input and callers may reuse
	// or mutate their slice.
	Send(ctx context.Context, sessionID string, input []harness.Content) (iter.Seq2[harness.Message, error], error)

	Cancel(sessionID string)

	Close() error
}

// Compactor checkpoints a session's context on demand; focus guides what the summary keeps.
type Compactor interface {
	Compact(ctx context.Context, sessionID, focus string) (harness.CompactResult, error)
}

// UnattendedAgent exposes an interaction policy independent of Agent/Plan mode.
// Adapters that only expose provider-defined modes may omit this interface.
type UnattendedAgent interface {
	Unattended(sessionID string) bool
	SetUnattended(ctx context.Context, sessionID string, enabled bool) error
}

var (
	ErrTurnInProgress = harness.ErrTurnInProgress
	ErrEmptyInput     = harness.ErrEmptyInput
)

// SessionForker copies an idle session into a new loaded one and returns its ID.
type SessionForker interface {
	ForkSession(ctx context.Context, id, title string) (string, error)
}

// SessionTurnForker forks a session through the turn of one of its inputs.
type SessionTurnForker interface {
	ForkSessionThrough(ctx context.Context, id, inputID, title string) (string, error)
}

type SessionLoadStreamer interface {
	LoadSessionStream(ctx context.Context, id string) iter.Seq2[[]harness.Message, error]
}

// HistorySnapshot is an atomic view of a session's retained messages. Revision
// changes only when existing history is rewritten (for example by compaction),
// not when messages are appended normally.
type HistorySnapshot struct {
	Messages []harness.Message
	Revision uint64
}

// HistorySnapshotProvider lets UIs distinguish an append from a history
// rewrite. Agents without rewrite behavior may continue to expose Messages.
type HistorySnapshotProvider interface {
	HistorySnapshot(sessionID string) HistorySnapshot
}

// HistoryVersion is a cheap discriminator for retained history. Revision
// changes on rewrites; MessageCount changes on ordinary appends.
type HistoryVersion struct {
	Revision     uint64
	MessageCount int
}

// HistoryVersionProvider lets polling UIs avoid cloning a complete history
// snapshot when neither its length nor rewrite revision changed.
type HistoryVersionProvider interface {
	HistoryVersion(sessionID string) HistoryVersion
}

// CommandProvider exposes agent-defined slash commands for clients that can
// present command discovery alongside their own local commands.
type CommandProvider interface {
	Commands(sessionID string) []Command
}

// SessionUpdateSource reports backend metadata updates that can arrive outside
// a Send stream (ACP configuration, commands, title, modes and usage).
// The callback runs after releasing adapter locks.
type SessionUpdateSource interface {
	SetSessionUpdateHandler(func(sessionID string))
}
