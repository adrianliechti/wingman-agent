package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/shell"
)

type TaskEntry struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	AgentType   string `json:"agent_type"`
	Status      string `json:"status"`
	Activity    string `json:"activity,omitempty"`
	Elapsed     int64  `json:"elapsed_seconds"`
	Seq         int    `json:"seq"`
}

func taskEntry(t task.Info) TaskEntry {
	return TaskEntry{
		ID:          t.ID,
		Description: t.Description,
		AgentType:   t.AgentType,
		Status:      string(t.Status),
		Activity:    t.Activity,
		Elapsed:     int64(t.Elapsed / time.Second),
		Seq:         t.Seq,
	}
}

const execTaskPrefix = "exec-"

func execTaskEntry(session shell.ExecSessionInfo) TaskEntry {
	description := session.Description
	if description == "" {
		description = session.Command
	}
	return TaskEntry{
		ID:          fmt.Sprintf("%s%d", execTaskPrefix, session.ID),
		Description: description,
		AgentType:   "command",
		Status:      string(task.StatusRunning),
		Activity:    "Command running",
		Elapsed:     int64(time.Since(session.Started) / time.Second),
		Seq:         1,
	}
}

func execSessionID(taskID string) (int, bool) {
	if !strings.HasPrefix(taskID, execTaskPrefix) {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimPrefix(taskID, execTaskPrefix))
	return id, err == nil && id > 0
}

func handleTasks(c *sessionController, w http.ResponseWriter, r *http.Request) {
	reg := c.backend.sessionTasks(c.ref.SessionID)
	out := []TaskEntry{}
	if reg != nil {
		for _, t := range reg.List() {
			out = append(out, taskEntry(t.Info()))
		}
	}
	if agent, ok := c.backend.agent.(*agent.Agent); ok {
		for _, session := range agent.ExecSessions(c.ref.SessionID) {
			out = append(out, execTaskEntry(session))
		}
	}
	writeJSON(w, out)
}

func handleTask(c *sessionController, w http.ResponseWriter, r *http.Request) {
	reg := c.backend.sessionTasks(c.ref.SessionID)
	if reg == nil {
		http.Error(w, "background agents unavailable", http.StatusNotFound)
		return
	}
	if t := reg.Get(r.PathValue("taskID")); t != nil {
		info := t.Info()
		writeJSON(w, struct {
			TaskEntry
			Result     string            `json:"result,omitempty"`
			Transcript []TranscriptEntry `json:"transcript"`
		}{
			TaskEntry:  taskEntry(info),
			Result:     info.Result,
			Transcript: transcriptEntries(t.PeekMessages()),
		})
		return
	}
	if id, ok := execSessionID(r.PathValue("taskID")); ok {
		if agent, agentOK := c.backend.agent.(*agent.Agent); agentOK {
			if session, found := agent.ExecSession(c.ref.SessionID, id); found {
				transcript := []TranscriptEntry{}
				if session.Output != "" {
					transcript = append(transcript, TranscriptEntry{
						ID:      fmt.Sprintf("exec-output-%d", id),
						Type:    "assistant",
						Content: session.Output,
					})
				}
				writeJSON(w, struct {
					TaskEntry
					Transcript []TranscriptEntry `json:"transcript"`
				}{TaskEntry: execTaskEntry(session), Transcript: transcript})
				return
			}
		}
	}
	http.Error(w, "task not found", http.StatusNotFound)
}

func handleTaskStop(c *sessionController, w http.ResponseWriter, r *http.Request) {
	reg := c.backend.sessionTasks(c.ref.SessionID)
	if reg == nil {
		http.Error(w, "background agents unavailable", http.StatusNotFound)
		return
	}
	taskID := r.PathValue("taskID")
	if reg.Get(taskID) != nil {
		if err := reg.Stop(taskID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if id, ok := execSessionID(taskID); ok {
		agent, agentOK := c.backend.agent.(*agent.Agent)
		if !agentOK {
			http.Error(w, "background command unavailable", http.StatusNotFound)
			return
		}
		if err := agent.StopExecSession(c.ref.SessionID, id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	c.backend.sendSession(c.ref.SessionID, Frame{Type: EvtTasksChanged})
	w.WriteHeader(http.StatusNoContent)
}
