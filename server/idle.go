package server

import (
	"maps"
	"slices"

	"github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
)

// busy reports whether closing the project would cut off work: a turn or
// queued input, an open question, a task, a schedule or a terminal.
func (s *Server) busy() bool {
	if s.terminals != nil && slices.ContainsFunc(s.terminals.List(), func(session *terminal.Session) bool { return !session.Exited() }) {
		return true
	}
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
	return starting || slices.ContainsFunc(runtimes, (*backendRuntime).busy)
}

func (b *backendRuntime) busy() bool {
	b.mu.Lock()
	sessions := maps.Clone(b.sessions)
	b.mu.Unlock()
	local, _ := b.agent.(*agent.Agent)
	for id, c := range sessions {
		c.mu.Lock()
		working := c.state.Phase != "" && c.state.Phase != "idle" || len(c.state.PendingInputs) > 0 || len(c.state.Prompts) > 0
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
		if schedules := local.Schedules(id); schedules != nil {
			if tasks, err := schedules.List(); err != nil || len(tasks) > 0 {
				return true
			}
		}
	}
	return false
}
