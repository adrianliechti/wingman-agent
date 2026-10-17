package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var ErrTaskTokenBudget = errors.New("task token budget reached; start a new turn to continue")
var ErrTaskTimeBudget = errors.New("task time limit reached; start a new turn to continue")

type taskBudgetKey struct{}
type taskBudget struct {
	limit  int64
	used   atomic.Int64
	cancel context.CancelCauseFunc
}

// Inline agents share the parent's allowance; detached tasks start their own.
// Usage is charged at the provider boundary, not again when child totals are
// credited to a parent. In-flight requests may exceed the remaining allowance.
func (c *Config) withTaskBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Value(taskBudgetKey{}) != nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	timeCancel := func() {}
	if c.MaxTaskDuration > 0 {
		ctx, timeCancel = context.WithTimeoutCause(ctx, c.MaxTaskDuration, ErrTaskTimeBudget)
	}
	ctx = context.WithValue(ctx, taskBudgetKey{}, &taskBudget{limit: c.MaxTaskTokens, cancel: cancel})
	return ctx, func() { timeCancel(); cancel(context.Canceled) }
}

func chargeTaskUsage(ctx context.Context, usage Usage) {
	b, _ := ctx.Value(taskBudgetKey{}).(*taskBudget)
	if b != nil && b.limit > 0 && b.used.Add(max(0, usage.InputTokens)+max(0, usage.OutputTokens)) >= b.limit {
		b.cancel(ErrTaskTokenBudget)
	}
}

func taskOutcomeError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); errors.Is(cause, ErrTaskTokenBudget) || errors.Is(cause, ErrTaskTimeBudget) {
		return errors.Join(cause, err)
	}
	return err
}

func (c *Config) loadTaskLimits() error {
	if value := strings.TrimSpace(os.Getenv("WINGMAN_TASK_MAX_TOKENS")); value != "" {
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || limit < 0 {
			return fmt.Errorf("WINGMAN_TASK_MAX_TOKENS must be a nonnegative integer")
		}
		c.MaxTaskTokens = limit
	}
	if value := strings.TrimSpace(os.Getenv("WINGMAN_TASK_TIMEOUT")); value != "" {
		limit, err := time.ParseDuration(value)
		if err != nil || limit < 0 {
			return fmt.Errorf("WINGMAN_TASK_TIMEOUT must be a nonnegative duration, such as 20m")
		}
		c.MaxTaskDuration = limit
	}
	return nil
}
