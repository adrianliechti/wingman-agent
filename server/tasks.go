package server

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
)

// taskOrigin marks the input that delivers background task results.
const taskOrigin = "task"

func (s *backendRuntime) sessionTasks(sessionID string) *task.Registry {
	ca, ok := s.agent.(*agent.Agent)
	if !ok {
		return nil
	}
	return ca.Tasks(sessionID)
}

// ensureTaskPump starts (once per registry) the goroutine that forwards
// background-agent completions of a session into its turn queue.
func (s *backendRuntime) ensureTaskPump(sessionID string) {
	if !s.Project.beginOperation() {
		return
	}
	reg := s.sessionTasks(sessionID)
	if reg == nil {
		s.Project.operations.Done()
		return
	}

	s.mu.Lock()
	if s.taskPumps == nil {
		s.taskPumps = map[*task.Registry]bool{}
	}
	if s.taskPumps[reg] {
		s.mu.Unlock()
		s.Project.operations.Done()
		return
	}
	s.taskPumps[reg] = true
	s.mu.Unlock()

	go func() {
		defer s.Project.operations.Done()
		defer func() {
			s.mu.Lock()
			delete(s.taskPumps, reg)
			s.mu.Unlock()
		}()

		for {
			select {
			case <-s.Project.ctx.Done():
				return
			case <-reg.Done():
				return
			case ev := <-reg.Events():
				batch := []task.Event{ev}
				for {
					select {
					case more := <-reg.Events():
						batch = append(batch, more)
						continue
					default:
					}
					break
				}
				// Retry failed delivery with the same input identity. Runtime
				// ownership stays fixed for the lifetime of this pump.
				for attempt := 0; !s.deliverTaskResults(sessionID, batch); attempt++ {
					if attempt >= 24 {
						fmt.Fprintf(os.Stderr, "giving up delivering %d background agent result(s) for session %s\n", len(batch), sessionID)
						break
					}
					select {
					case <-s.Project.ctx.Done():
						return
					case <-reg.Done():
						return
					case <-time.After(5 * time.Second):
					}
				}
			}
		}
	}()
}

func (s *backendRuntime) deliverTaskResults(sessionID string, batch []task.Event) bool {
	c := s.session(sessionID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	deleted := c.state.Status == sessionDeleted
	c.mu.Unlock()
	if deleted || s.Project.ctx.Err() != nil || len(batch) == 0 {
		return true
	}
	receiptID := fmt.Sprintf("task:%s:%d", batch[0].ID, batch[0].Seq)
	if _, ok := c.deliveredTasks[receiptID]; ok {
		return true
	}

	s.sendSession(sessionID, Frame{Type: EvtTasksChanged})

	var blocks []string
	for _, ev := range batch {
		blocks = append(blocks, ev.Notification())
	}

	turns := s.turns
	if turns == nil {
		return false
	}

	first := batch[0]
	_, err := turns.Submit(s.Project.ctx, sessionID, code.TurnInput{
		ID:     fmt.Sprintf("task-%s-%d", first.ID, first.Seq),
		Intent: code.TurnInputSteer,
		Origin: taskOrigin,
		Content: []harness.Content{{
			Text:   strings.Join(blocks, "\n\n"),
			Hidden: true,
		}},
	})
	if err != nil {
		if errors.Is(err, code.ErrDuplicateInput) {
			return true
		}
		fmt.Fprintf(os.Stderr, "deliver background agent results (%s): %v\n", sessionID, err)
		return false
	}
	c.deliveredTasks[receiptID] = true
	return true
}

func (s *backendRuntime) sessionSchedules(sessionID string) schedule.Store {
	ca, ok := s.agent.(*agent.Agent)
	if !ok {
		return nil
	}
	return ca.Schedules(sessionID)
}
