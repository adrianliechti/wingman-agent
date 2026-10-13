//go:build e2e

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type webTaskRelease struct {
	done chan struct{}
	once sync.Once
}

func (m *webE2EModel) taskRelease(name string) *webTaskRelease {
	release, _ := m.taskReleases.LoadOrStore(name, &webTaskRelease{done: make(chan struct{})})
	return release.(*webTaskRelease)
}

func e2eTaskCall(id, name string, args map[string]any) map[string]any {
	encoded, _ := json.Marshal(args)
	return map[string]any{
		"type": "function_call", "id": "fc_" + id, "call_id": id,
		"name": name, "arguments": string(encoded), "status": "completed",
	}
}

func emitE2ETaskCalls(w http.ResponseWriter, items ...any) {
	for i, item := range items {
		emitE2EEvent(w, map[string]any{
			"type": "response.output_item.done", "sequence_number": i + 1,
			"output_index": i, "item": item,
		})
	}
	emitE2EEvent(w, map[string]any{
		"type": "response.completed", "sequence_number": len(items) + 1,
		"response": map[string]any{"output": items},
	})
}

// The browser uses real tools, task registries, schedules and REST endpoints;
// only model output and the release of long-running child requests are faked.
func (m *webE2EModel) handleTaskFixture(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var req struct {
		Input []struct {
			Type    string                  `json:"type"`
			Role    string                  `json:"role"`
			Content []struct{ Text string } `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	var users []string
	hasResult := false
	for _, item := range req.Input {
		hasResult = hasResult || item.Type == "function_call_output"
		if item.Role == "user" {
			for _, content := range item.Content {
				users = append(users, content.Text)
			}
		}
	}
	for _, input := range users {
		if !strings.HasPrefix(input, "e2e task child ") {
			continue
		}
		name := strings.TrimPrefix(input, "e2e task child ")
		if !hasResult {
			emitE2ETaskCalls(w, map[string]any{
				"type": "message", "id": "msg_inspect", "role": "assistant", "status": "completed", "phase": "commentary",
				"content": []any{map[string]any{"type": "output_text", "text": "Inspecting " + name + " files.", "annotations": []any{}}},
			}, e2eTaskCall("e2e_read", "read", map[string]any{"file_path": "readme-preview.md"}))
			return true
		}
		w.(http.Flusher).Flush()
		select {
		case <-m.taskRelease(name).done:
			emitE2ETextResponse(w, "msg_child_done", "Completed "+name+" review.")
		case <-r.Context().Done():
		}
		return true
	}
	mode := ""
	for _, input := range users {
		switch input {
		case "task visibility fixture":
			mode = "visibility"
		case "task cancellation fixture":
			mode = "cancellation"
		case "task schedule fixture":
			mode = "schedule"
		}
	}
	if mode == "" {
		return false
	}
	if len(users) > 0 && strings.Contains(users[len(users)-1], "Scheduled task") && strings.Contains(users[len(users)-1], "e2e scheduled check") {
		emitE2ETextResponse(w, "msg_scheduled_done", "Scheduled check completed.")
		return true
	}
	if hasResult {
		emitE2ETextResponse(w, "msg_parent_done", "Delegation finished.")
		return true
	}
	if mode == "schedule" {
		emitE2ETaskCalls(w, e2eTaskCall("e2e_schedule", "schedule", map[string]any{
			"action": "create", "prompt": "e2e scheduled check",
			"schedule": time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339),
		}))
		return true
	}
	var calls []any
	if mode == "visibility" {
		calls = append(calls, e2eTaskCall("e2e_schedule", "schedule", map[string]any{
			"action": "create", "prompt": "Scheduled review", "schedule": "every 1h",
		}))
	}
	for _, child := range []struct {
		name, description string
		background        bool
	}{{"async", "Asynchronous review", true}, {"sync", "Synchronous review", false}} {
		calls = append(calls, e2eTaskCall("e2e_"+child.name, "agent", map[string]any{
			"description": child.description, "agent_type": "code-reviewer",
			"prompt": fmt.Sprintf("e2e task child %s %s", child.name, mode), "background": child.background,
		}))
	}
	emitE2ETaskCalls(w, calls...)
	return true
}
