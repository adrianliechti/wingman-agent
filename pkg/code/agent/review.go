package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

const (
	reviewConsecutiveDenials = 3
	reviewRecentDenials      = 10
	reviewRecentWindow       = 50
	reviewArgumentBytes      = 8 * 1024
)

// review decides approvals: permission hooks, then the reviewer; the user only when review fails.
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
		ok, err := a.confirm(ctx, fmt.Sprintf("%s\n\nAutomatic review failed: %v", req.Message, err))
		return tool.Verdict{Allowed: ok}, err
	}
	turn := s.reviewTurn(ctx)
	if review.Allowed() {
		s.reviews.record(turn, false)
		return tool.Verdict{Allowed: true}, nil
	}
	reason := review.Rejection()
	if consecutive, recent, tripped := s.reviews.record(turn, true); tripped {
		reason += fmt.Sprintf("\nAutomatic review rejected too many actions this turn (%d consecutive, %d in the last %d); the turn was interrupted.", consecutive, recent, reviewRecentWindow)
		if origin := tool.BackgroundOrigin(ctx); origin != "" && s.tasks != nil {
			_ = s.tasks.Stop(origin)
		} else {
			s.cancel(nil)
		}
	}
	return tool.Verdict{Reason: reason}, nil
}

func (s *sessionState) reviewTurn(ctx context.Context) string {
	turn := hook.RuntimeFromContext(ctx).TurnID
	if turn == "" {
		return ""
	}
	if origin := tool.BackgroundOrigin(ctx); origin != "" {
		return origin + ":" + turn
	}
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.cancelFn != nil {
		return fmt.Sprintf("turn-%d", s.cancelGen)
	}
	return turn
}

// permissionHooks reports whether a PermissionRequest hook allowed or denied the current call.
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

// reviewBreaker stops a turn whose actions keep getting denied, so an injected agent cannot probe indefinitely.
type reviewBreaker struct {
	mu    sync.Mutex
	turns map[string]*reviewTurn
}

type reviewTurn struct {
	consecutive int
	recent      []bool
	tripped     bool
}

// record counts per turn; reviews outside a turn, such as schedule gates, never trip it.
func (b *reviewBreaker) record(turn string, denied bool) (consecutive, recent int, tripped bool) {
	if turn == "" {
		return 0, 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turns == nil {
		b.turns = map[string]*reviewTurn{}
	}
	state := b.turns[turn]
	if state == nil {
		if len(b.turns) >= 16 {
			clear(b.turns)
		}
		state = &reviewTurn{}
		b.turns[turn] = state
	}
	state.recent = append(state.recent, denied)
	if len(state.recent) > reviewRecentWindow {
		state.recent = state.recent[1:]
	}
	if !denied {
		state.consecutive = 0
		return 0, 0, false
	}
	state.consecutive++
	for _, d := range state.recent {
		if d {
			recent++
		}
	}
	if state.tripped || (state.consecutive < reviewConsecutiveDenials && recent < reviewRecentDenials) {
		return state.consecutive, recent, false
	}
	state.tripped = true
	return state.consecutive, recent, true
}
