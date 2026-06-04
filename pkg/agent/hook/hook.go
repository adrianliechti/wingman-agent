package hook

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type PreToolUse func(ctx context.Context, call tool.ToolCall) (string, error)

type PostToolUse func(ctx context.Context, call tool.ToolCall, result string) (string, error)

type Hooks struct {
	PreToolUse  []PreToolUse
	PostToolUse []PostToolUse
}
