package websearch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type searchApproval struct {
	mu      sync.Mutex
	allowed bool
	pending *pendingApproval
}

type pendingApproval struct {
	done chan struct{}
	err  error
}

func approveSearch(ctx context.Context, elicit *tool.Elicitation, approval *searchApproval, query string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if elicit == nil || elicit.Confirm == nil {
			return nil
		}
		approval.mu.Lock()
		if approval.allowed {
			approval.mu.Unlock()
			return nil
		}
		if pending := approval.pending; pending != nil {
			approval.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-pending.done:
				// A canceled prompt owner must not cancel independent searches.
				if errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, context.DeadlineExceeded) {
					continue
				}
				return pending.err
			}
		}
		pending := &pendingApproval{done: make(chan struct{})}
		approval.pending = pending
		approval.mu.Unlock()

		displayQuery := strings.Join(strings.Fields(query), " ")
		ok, err := elicit.Confirm(ctx, fmt.Sprintf("Allow web searches via DuckDuckGo for this session?\nFirst query: %s", displayQuery))
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if err == nil && !ok {
			err = fmt.Errorf("the user declined web search")
		}
		approval.mu.Lock()
		approval.allowed = err == nil
		pending.err = err
		approval.pending = nil
		close(pending.done)
		approval.mu.Unlock()
		return err
	}
}
