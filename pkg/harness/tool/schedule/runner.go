package schedule

import (
	"context"
	"errors"
	"slices"
	"time"
)

const (
	// scheduleWaitMax bounds the idle sleep so drift in long waits self-heals.
	scheduleWaitMax = time.Minute
	// scheduleWaitMin keeps an overdue task from busy-spinning the loop.
	scheduleWaitMin = time.Second
)

// Fire describes one committed schedule occurrence. GateOutput is additional
// context from the optional gate. Sequence distinguishes recurring occurrences.
type Fire struct {
	Task       Task
	GateOutput string
	Sequence   uint64
}

// Runner dispatches schedules from one store. The host explicitly starts it
// and owns delivery into its conversation queue. Use one runner per store;
// Run and FireDue must not be called concurrently. Without Deliver, tasks
// remain pending and gates are not executed.
type Runner struct {
	Store   ObservableStore
	Gate    func(context.Context, Task) (wake bool, output string, err error)
	Deliver func(context.Context, Fire)
}

// Run waits for deadlines or store changes until ctx is canceled. Gate and
// Deliver callbacks must honor cancellation.
func (r *Runner) Run(ctx context.Context) {
	for {
		timer := time.NewTimer(r.nextWait())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.Store.Changed():
			timer.Stop()
		case <-timer.C:
			r.FireDue(ctx)
		}
	}
}

func (r *Runner) nextWait() time.Duration {
	tasks, err := r.Store.List()
	if err != nil {
		return scheduleWaitMax
	}

	now := time.Now()
	wait := scheduleWaitMax

	for _, t := range tasks {
		if next := NextAttempt(t, now); !next.IsZero() {
			wait = min(wait, next.Sub(now))
		}
	}

	return max(wait, scheduleWaitMin)
}

// FireDue checks and dispatches currently due tasks once.
func (r *Runner) FireDue(ctx context.Context) {
	tasks, err := r.Store.List()
	if err != nil || len(tasks) == 0 {
		return
	}

	now := time.Now()

	for _, t := range tasks {
		if ctx.Err() != nil {
			return
		}
		if !IsDue(t, now) {
			continue
		}
		r.fire(ctx, t)
	}
}

// errTaskChanged aborts a store mutation without rewriting an obsolete snapshot.
var errTaskChanged = errors.New("scheduled task changed during execution")

func (r *Runner) fire(ctx context.Context, t Task) {
	if ctx.Err() != nil || r.Deliver == nil {
		return
	}
	// An earlier gate in this batch may have paused or replaced this task.
	tasks, err := r.Store.List()
	if err != nil || !slices.ContainsFunc(tasks, func(current Task) bool { return sameTask(current, t) && current.Status == StatusActive }) {
		return
	}
	wake := true
	gateOutput := ""
	var gateErr error

	if r.Gate != nil {
		wake, gateOutput, gateErr = r.Gate(ctx, t)
	}

	if ctx.Err() != nil {
		return
	}

	now := time.Now()

	var fireSeq uint64
	if err := r.Store.Mutate(func(tasks []Task) ([]Task, error) {
		// Cancellation can arrive while waiting for the store's write lock.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for i := range tasks {
			if tasks[i].ID != t.ID {
				continue
			}
			if !sameTask(tasks[i], t) {
				return nil, errTaskChanged
			}
			tasks[i].FireSeq++
			fireSeq = tasks[i].FireSeq
			if IsOneTime(t.Schedule) {
				return slices.Delete(tasks, i, i+1), nil
			}
			tasks[i].LastRun = &now
			if gateErr != nil {
				tasks[i].Failures++
				tasks[i].LastAttempt = &now
			} else {
				tasks[i].Failures = 0
				tasks[i].LastAttempt = nil
			}
			return tasks, nil
		}
		return nil, errTaskChanged
	}); err != nil {
		return
	}

	if wake {
		r.Deliver(ctx, Fire{Task: t, GateOutput: gateOutput, Sequence: fireSeq})
	}
}

// Compare the complete observed state, including the occurrence counter, so a
// gate cannot consume an edited task or overwrite another runner's progress.
func sameTask(a, b Task) bool {
	return a.ID == b.ID && a.Prompt == b.Prompt && a.Schedule == b.Schedule &&
		a.Script == b.Script && a.Status == b.Status && a.CreatedAt.Equal(b.CreatedAt) &&
		a.Failures == b.Failures && a.FireSeq == b.FireSeq &&
		sameOptionalTime(a.LastRun, b.LastRun) && sameOptionalTime(a.LastAttempt, b.LastAttempt)
}

func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
