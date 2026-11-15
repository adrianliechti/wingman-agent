package server

import (
	"net/http"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
)

type ScheduleEntry struct {
	ID       string `json:"id"`
	Prompt   string `json:"prompt"`
	Schedule string `json:"schedule"`
	Status   string `json:"status"`
	Script   bool   `json:"script,omitempty"`
	NextRun  string `json:"next_run,omitempty"`
	NextIn   string `json:"next_in,omitempty"`
	LastRun  string `json:"last_run,omitempty"`
	Failures int    `json:"failures,omitempty"`
}

func (s *backendRuntime) sessionSchedules(sessionID string) schedule.Store {
	ca, ok := s.agent.(*agent.Agent)
	if !ok {
		return nil
	}
	return ca.Schedules(sessionID)
}

func (c *sessionController) handleSchedules(w http.ResponseWriter, r *http.Request) {
	store := c.backend.sessionSchedules(c.ref.SessionID)
	if store == nil {
		writeJSON(w, []ScheduleEntry{})
		return
	}

	tasks, err := store.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	out := []ScheduleEntry{}

	for _, t := range tasks {
		entry := ScheduleEntry{
			ID:       t.ID,
			Prompt:   t.Prompt,
			Schedule: t.Schedule,
			Status:   t.Status,
			Script:   t.Script != "",
			Failures: t.Failures,
		}

		if next := schedule.NextAttempt(t, now); !next.IsZero() {
			entry.NextRun = next.Local().Format(time.RFC3339)
			entry.NextIn = schedule.Relative(next, now)
		}

		if t.LastRun != nil {
			entry.LastRun = t.LastRun.Local().Format(time.RFC3339)
		}

		out = append(out, entry)
	}

	writeJSON(w, out)
}

func (c *sessionController) handleScheduleDelete(w http.ResponseWriter, r *http.Request) {
	store := c.backend.sessionSchedules(c.ref.SessionID)
	if store == nil {
		http.Error(w, "scheduled tasks unavailable", http.StatusNotFound)
		return
	}

	id := r.PathValue("scheduleID")

	err := store.Mutate(func(tasks []schedule.Task) ([]schedule.Task, error) {
		i, err := schedule.Find(tasks, id)
		if err != nil {
			return nil, err
		}
		return append(tasks[:i:i], tasks[i+1:]...), nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	c.backend.sendSession(c.ref.SessionID, Frame{Type: EvtTasksChanged})
	w.WriteHeader(http.StatusNoContent)
}

func (c *sessionController) handleSchedulePause(w http.ResponseWriter, r *http.Request) {
	c.handleScheduleStatus(w, r, schedule.StatusPaused)
}

func (c *sessionController) handleScheduleResume(w http.ResponseWriter, r *http.Request) {
	c.handleScheduleStatus(w, r, schedule.StatusActive)
}

func (c *sessionController) handleScheduleStatus(w http.ResponseWriter, r *http.Request, status string) {
	store := c.backend.sessionSchedules(c.ref.SessionID)
	if store == nil {
		http.Error(w, "scheduled tasks unavailable", http.StatusNotFound)
		return
	}

	id := r.PathValue("scheduleID")
	err := store.Mutate(func(tasks []schedule.Task) ([]schedule.Task, error) {
		i, err := schedule.Find(tasks, id)
		if err != nil {
			return nil, err
		}
		tasks[i].Status = status
		return tasks, nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	c.backend.sendSession(c.ref.SessionID, Frame{Type: EvtTasksChanged})
	w.WriteHeader(http.StatusNoContent)
}
