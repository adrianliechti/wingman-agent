package server

import (
	"maps"
	"slices"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
)

// busy reports whether closing the project would cut off work: a turn, queued
// input its agent does not save, an open question, a task, a schedule or a terminal.
func (s *Project) busy() bool {
	if s.terminals != nil && slices.ContainsFunc(s.terminals.List(), func(session *terminal.Session) bool { return !session.Exited() }) {
		return true
	}
	return s.agentsBusy(true)
}

// working reports whether the project works right now: what busy counts,
// except schedules and terminals that wait at their prompt.
func (s *Project) working() bool {
	if s.terminals != nil && slices.ContainsFunc(s.terminals.List(), (*terminal.Session).HasForegroundProcess) {
		return true
	}
	return s.agentsBusy(false)
}

// Idle reports whether the server's host could stop without cutting anyone
// off: no client follows it and no project works. Schedules wait for the next
// start, as they do when the host is closed.
func (s *Server) Idle() bool {
	h := s.Project.host
	h.mu.Lock()
	clients := len(h.clients)
	h.mu.Unlock()
	return clients == 0 && !slices.ContainsFunc(h.loaded(), (*Project).working)
}

func (s *Project) agentsBusy(schedules bool) bool {
	s.backendsMu.Lock()
	var runtimes []*backendRuntime
	starting := false
	for _, entry := range s.backends {
		if entry.runtime == nil {
			starting = true
		} else {
			runtimes = append(runtimes, entry.runtime)
		}
	}
	s.backendsMu.Unlock()
	return starting || slices.ContainsFunc(runtimes, func(b *backendRuntime) bool { return b.busy(schedules) })
}

func (b *backendRuntime) busy(schedules bool) bool {
	b.mu.Lock()
	sessions := maps.Clone(b.sessions)
	b.mu.Unlock()
	local, _ := b.agent.(*agent.Agent)
	_, saved := b.agent.(code.TurnQueueStore) // a saved queue waits for its session to reopen
	for id, c := range sessions {
		c.mu.Lock()
		working := c.state.Phase != "" && c.state.Phase != "idle" || len(c.state.PendingInputs) > 0 && !saved || len(c.state.Prompts) > 0
		c.mu.Unlock()
		if working {
			return true
		}
		if local == nil {
			continue
		}
		if tasks := local.Tasks(id); tasks != nil && slices.ContainsFunc(tasks.List(), func(t *task.Task) bool { return t.Status() == task.StatusRunning }) {
			return true
		}
		if len(local.ExecSessions(id)) > 0 {
			return true
		}
		// A schedule only fires while its session's project is open.
		if store := local.Schedules(id); schedules && store != nil {
			if tasks, err := store.List(); err != nil || len(tasks) > 0 {
				return true
			}
		}
	}
	return false
}
