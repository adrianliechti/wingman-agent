package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

const (
	reviewConsecutiveDenials = 3
	reviewRecentDenials      = 10
	reviewRecentWindow       = 50
	reviewArgumentBytes      = 8 * 1024
)

// review decides an action approval in Agent mode: permission hooks first,
// then the automatic reviewer. The user is asked only when review fails.
func (a *Agent) review(ctx context.Context, req tool.Approval) (tool.Verdict, error) {
	ctx = a.promptContext(ctx)
	s := a.session(code.SessionIDFromContext(ctx))
	if s == nil {
		ok, err := a.confirm(ctx, req.Message)
		return tool.Verdict{Allowed: ok}, err
	}
	if allowed, decided := s.permissionHooks(ctx); decided {
		return tool.Verdict{Allowed: allowed}, nil
	}
	if s.unattended.Load() {
		return tool.Verdict{Allowed: true}, nil
	}

	action := maps.Clone(req.Action)
	if action == nil {
		action = map[string]any{}
	}
	action["workspace"] = a.workspace.RootPath
	if call, ok := hook.ToolCallFromContext(ctx); ok {
		action["tool"] = call.Name
		switch {
		case len(call.Args) > reviewArgumentBytes:
			action["arguments"] = call.Args[:reviewArgumentBytes] + "\n[... arguments truncated ...]"
		case json.Valid([]byte(call.Args)):
			action["arguments"] = json.RawMessage(call.Args)
		case call.Args != "":
			action["arguments"] = call.Args
		}
	}

	review, err := s.aa.ReviewAction(ctx, action)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return tool.Verdict{}, ctxErr
		}
		ok, err := a.askUser(ctx, fmt.Sprintf("%s\n\nAutomatic review failed: %v", req.Message, err))
		return tool.Verdict{Allowed: ok}, err
	}
	if review.Allowed() {
		s.reviews.record(false)
		return tool.Verdict{Allowed: true}, nil
	}
	reason := review.Rejection()
	if consecutive, recent, tripped := s.reviews.record(true); tripped {
		reason += fmt.Sprintf("\nAutomatic review rejected too many actions this turn (%d consecutive, %d in the last %d); the turn was interrupted.", consecutive, recent, reviewRecentWindow)
		s.cancel()
	}
	return tool.Verdict{Reason: reason}, nil
}

// permissionHooks reports a PermissionRequest hook decision for the current
// tool call; decided is false when no hook allowed or denied it.
func (s *sessionState) permissionHooks(ctx context.Context) (allowed, decided bool) {
	call, ok := hook.ToolCallFromContext(ctx)
	if !ok {
		return false, false
	}
	for _, run := range s.aa.Hooks.PermissionRequest {
		outcome, err := run(ctx, call)
		if err != nil {
			continue
		}
		switch outcome.Behavior {
		case hook.PermissionDeny:
			return false, true
		case hook.PermissionAllow:
			allowed = true
		}
	}
	return allowed, allowed
}

// reviewBreaker interrupts a turn whose actions keep getting denied, so a
// confused or injected agent cannot probe the reviewer indefinitely.
type reviewBreaker struct {
	mu          sync.Mutex
	consecutive int
	recent      []bool
	tripped     bool
}

func (b *reviewBreaker) record(denied bool) (consecutive, recent int, tripped bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = append(b.recent, denied)
	if len(b.recent) > reviewRecentWindow {
		b.recent = b.recent[1:]
	}
	if !denied {
		b.consecutive = 0
		return 0, 0, false
	}
	b.consecutive++
	for _, d := range b.recent {
		if d {
			recent++
		}
	}
	if b.tripped || (b.consecutive < reviewConsecutiveDenials && recent < reviewRecentDenials) {
		return b.consecutive, recent, false
	}
	b.tripped = true
	return b.consecutive, recent, true
}

func (b *reviewBreaker) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive, b.recent, b.tripped = 0, nil, false
}
