package shell

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type approvals struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newApprovals() *approvals {
	return &approvals{seen: map[string]bool{}}
}

func confirmDangerous(ctx context.Context, elicit *tool.Elicitation, appr *approvals, args map[string]any) error {
	if elicit == nil || elicit.Confirm == nil || ClassifyEffect(args) != tool.EffectDangerous {
		return nil
	}

	command, _ := args["command"].(string)
	key := strings.Join(strings.Fields(command), " ")

	appr.mu.Lock()
	seen := appr.seen[key]
	appr.mu.Unlock()

	if seen {
		return nil
	}

	approved, err := elicit.Confirm(ctx, "❯ "+command)

	if err != nil {
		return fmt.Errorf("failed to get user approval: %w", err)
	}

	if !approved {
		return fmt.Errorf("command execution denied by user")
	}

	appr.mu.Lock()
	appr.seen[key] = true
	appr.mu.Unlock()

	return nil
}
