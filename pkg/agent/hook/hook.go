package hook

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type PreToolUse func(ctx context.Context, call tool.ToolCall) (string, error)

type PostToolUse func(ctx context.Context, call tool.ToolCall, result string) (string, error)

// UserPromptSubmit runs before a turn starts. A non-empty return value is
// injected as additional hidden context; an error blocks the turn.
type UserPromptSubmit func(ctx context.Context, prompt string) (string, error)

// SessionStart runs once before an agent's first turn. A non-empty return
// value is injected as hidden context.
type SessionStart func(ctx context.Context) (string, error)

// SessionEnd runs when the session closes.
type SessionEnd func(ctx context.Context)

// SubagentStop observes a completed subagent run.
type SubagentStop func(ctx context.Context, agentType, result string)

// PreCompact runs before proactive compaction; an error skips the compaction.
// Reactive overflow compaction is never blocked.
type PreCompact func(ctx context.Context) error

type Hooks struct {
	PreToolUse       []PreToolUse
	PostToolUse      []PostToolUse
	UserPromptSubmit []UserPromptSubmit
	SessionStart     []SessionStart
	SessionEnd       []SessionEnd
	SubagentStop     []SubagentStop
	PreCompact       []PreCompact
}
