package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
)

const (
	taskChildPrompt = "background probe briefing"
	taskChildResult = "PROBE-RESULT-42"
	taskNoticeTag   = "<task-notification>"
)

// taskModel replaces the model service. The parent turn launches one
// background agent through the real agent tool; that agent's own request
// answers with taskChildResult once release is closed.
type taskModel struct {
	release chan struct{}
	// keepTurnOpen makes the parent call tools until the result reaches it,
	// so the background agent finishes while the launching turn is running.
	keepTurnOpen bool

	mu     sync.Mutex
	parent [][]string // user texts of every parent request, in order
	polls  int
}

// taskRequestUsers returns the user texts of one model request, without the
// session context preamble, and whether it already carries a tool result.
func taskRequestUsers(r *http.Request) (users []string, toolResult bool) {
	var req struct {
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	for _, item := range req.Input {
		toolResult = toolResult || item.Type == "function_call_output"
		if item.Role != "user" {
			continue
		}
		for _, content := range item.Content {
			if !strings.HasPrefix(content.Text, "[Session context]") {
				users = append(users, content.Text)
			}
		}
	}
	return users, toolResult
}

func (m *taskModel) respond(w http.ResponseWriter, r *http.Request) {
	users, launched := taskRequestUsers(r)
	child, notified := false, false
	for _, text := range users {
		child = child || strings.Contains(text, taskChildPrompt)
		notified = notified || strings.Contains(text, taskNoticeTag)
	}
	if child {
		select {
		case <-m.release:
			writeTaskText(w, taskChildResult)
		case <-r.Context().Done():
		}
		return
	}

	m.mu.Lock()
	m.parent = append(m.parent, users)
	poll := 0
	if launched && !notified && m.keepTurnOpen {
		m.polls++
		poll = m.polls
	}
	m.mu.Unlock()

	switch {
	case notified:
		writeTaskText(w, "relayed")
	case !launched:
		writeTaskCall(w, "launch", "agent", map[string]any{
			"description": "Probe the workspace", "agent_type": "explore",
			"prompt": taskChildPrompt, "background": true,
		})
	case poll > 0 && poll <= 150:
		// Bounded, so a result that never arrives fails the test instead of
		// running into the harness's own turn limit.
		time.Sleep(5 * time.Millisecond)
		writeTaskCall(w, fmt.Sprintf("wait-%d", poll), "glob", map[string]any{"pattern": "*"})
	default:
		writeTaskText(w, "launched")
	}
}

func (m *taskModel) parentRequests() [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]string(nil), m.parent...)
}

func writeTaskText(w http.ResponseWriter, text string) {
	writeContractSSE(w, completedEvent(1, 5, 0, 0, 1, 0, []map[string]any{streamingTextItem(text)}))
}

func writeTaskCall(w http.ResponseWriter, id, name string, args map[string]any) {
	encoded, _ := json.Marshal(args)
	writeContractSSE(w, map[string]any{
		"type": "response.output_item.done", "sequence_number": 1, "output_index": 0,
		"item": map[string]any{
			"type": "function_call", "id": "fc-" + id, "call_id": id,
			"name": name, "arguments": string(encoded), "status": "completed",
		},
	})
	writeContractSSE(w, completedEvent(2, 5, 0, 0, 1, 0, nil))
}

// taskNotices returns the background-result blocks among one request's user texts.
func taskNotices(users []string) []string {
	var notices []string
	for _, text := range users {
		if strings.Contains(text, taskNoticeTag) {
			notices = append(notices, text)
		}
	}
	return notices
}

type taskClient struct {
	streamingClient
	userMu sync.Mutex
	user   strings.Builder
}

func (c *taskClient) SessionUpdate(ctx context.Context, notification acpsdk.SessionNotification) error {
	if chunk := notification.Update.UserMessageChunk; chunk != nil && chunk.Content.Text != nil {
		c.userMu.Lock()
		c.user.WriteString(chunk.Content.Text.Text + "\n")
		c.userMu.Unlock()
	}
	return c.streamingClient.SessionUpdate(ctx, notification)
}

func (c *taskClient) userText() string {
	c.userMu.Lock()
	defer c.userMu.Unlock()
	return c.user.String()
}

func taskPrompt(ctx context.Context, t *testing.T, conn *acpsdk.ClientSideConnection, id acpsdk.SessionId, text string) {
	t.Helper()
	result, err := conn.Prompt(ctx, acpsdk.PromptRequest{SessionId: id, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock(text)}})
	if err != nil || result.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("prompt %q: result=%+v error=%v", text, result, err)
	}
}

func heldTaskEvents(p *taskPump) []task.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]task.Event(nil), p.held...)
}

func waitTaskCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func taskPumpStopped(p *taskPump) bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func TestACPBackgroundResultReachesActiveTurn(t *testing.T) {
	release := make(chan struct{})
	close(release)
	model := &taskModel{release: release, keepTurnOpen: true}
	client := &taskClient{}
	s, conn, id := e2eConnection(t, client, model.respond)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// One prompt: the agent it launches finishes while the turn is running,
	// and the model has to see the result before that same turn ends.
	taskPrompt(ctx, t, conn, id, "launch the probe")

	requests := model.parentRequests()
	notices := taskNotices(requests[len(requests)-1])
	if len(notices) != 1 || !strings.Contains(notices[0], taskChildResult) || !strings.Contains(notices[0], "Background agent") {
		t.Fatalf("final request of the turn carried notices %q; all requests: %q", notices, requests)
	}
	if strings.Count(notices[0], taskNoticeTag) != 1 {
		t.Fatalf("result was delivered more than once: %q", notices[0])
	}
	if got := client.receivedText(); !strings.HasSuffix(got, "relayed") {
		t.Fatalf("turn output = %q, want the model's reply to the result", got)
	}
	if strings.Contains(client.receivedText(), taskNoticeTag) || strings.Contains(client.userText(), taskNoticeTag) {
		t.Fatalf("hidden notification was shown to the client: agent=%q user=%q", client.receivedText(), client.userText())
	}
	if held := heldTaskEvents(s.lookupSession(id).tasks); len(held) != 0 {
		t.Fatalf("delivered result is still held: %+v", held)
	}
}

func TestACPBackgroundResultWaitsForNextPrompt(t *testing.T) {
	model := &taskModel{release: make(chan struct{})}
	client := &taskClient{}
	s, conn, id := e2eConnection(t, client, model.respond)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// The launching turn ends while the background agent is still running.
	taskPrompt(ctx, t, conn, id, "launch the probe")
	sess := s.lookupSession(id)
	pump := sess.tasks
	before := len(model.parentRequests())

	// ACP has no turn to deliver into: the result is held, not dropped, and
	// the server does not start a turn the client never asked for.
	close(model.release)
	waitTaskCondition(t, "the idle result to be held", func() bool { return len(heldTaskEvents(pump)) == 1 })
	if after := len(model.parentRequests()); after != before {
		t.Fatalf("server ran %d model request(s) without a client prompt", after-before)
	}

	// A prompt whose turn never starts gives the result back.
	if _, err := s.Prompt(ctx, acpsdk.PromptRequest{SessionId: id}); err == nil {
		t.Fatal("empty prompt started a turn")
	}
	if held := heldTaskEvents(pump); len(held) != 1 {
		t.Fatalf("failed prompt lost the held result: %+v", held)
	}

	// The next prompt carries it to the model, next to the user's own text.
	taskPrompt(ctx, t, conn, id, "anything new?")
	requests := model.parentRequests()
	last := requests[len(requests)-1]
	notices := taskNotices(last)
	if len(notices) != 1 || !strings.Contains(notices[0], taskChildResult) || strings.Count(notices[0], taskNoticeTag) != 1 {
		t.Fatalf("next prompt carried notices %q; request users: %q", notices, last)
	}
	if !strings.Contains(strings.Join(last, "\n"), "anything new?") {
		t.Fatalf("next prompt lost the user's text: %q", last)
	}
	if held := heldTaskEvents(pump); len(held) != 0 {
		t.Fatalf("delivered result is still held: %+v", held)
	}

	// Delivered once: a later prompt only sees it in history.
	taskPrompt(ctx, t, conn, id, "thanks")
	requests = model.parentRequests()
	if notices := taskNotices(requests[len(requests)-1]); len(notices) != 1 {
		t.Fatalf("result was delivered again: %q", notices)
	}

	// Something else finishes while the session is idle again.
	sess.agent.Tasks(string(id)).Publish(task.Event{ID: "exec-9", Kind: task.KindCommand, AgentType: "exec", Description: "make", Seq: 1, Status: task.StatusDone, Result: "exit status 0"})
	waitTaskCondition(t, "the command exit to be held", func() bool { return len(heldTaskEvents(pump)) == 1 })

	// Replaying history shows the user's prompts without the injected result.
	cwd := sess.workspace.ws.RootPath
	if _, err := conn.LoadSession(ctx, acpsdk.LoadSessionRequest{SessionId: id, Cwd: cwd, McpServers: []acpsdk.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	waitTaskCondition(t, "the replayed history", func() bool { return strings.Contains(client.userText(), "thanks") })
	if replayed := client.userText(); !strings.Contains(replayed, "anything new?") || strings.Contains(replayed, taskNoticeTag) {
		t.Fatalf("replayed user messages = %q", replayed)
	}

	// Loading replaced the session entry. Each pump ends with its entry, and
	// what the old one still held moved to its successor.
	reloaded := s.lookupSession(id).tasks
	if reloaded == pump || !taskPumpStopped(pump) || taskPumpStopped(reloaded) {
		t.Fatalf("reload: same pump=%t old stopped=%t new stopped=%t", reloaded == pump, taskPumpStopped(pump), taskPumpStopped(reloaded))
	}
	if held := heldTaskEvents(reloaded); len(held) != 1 || held[0].ID != "exec-9" {
		t.Fatalf("reload lost the held command exit: %+v", held)
	}
	if _, err := conn.CloseSession(ctx, acpsdk.CloseSessionRequest{SessionId: id}); err != nil {
		t.Fatal(err)
	}
	if !taskPumpStopped(reloaded) {
		t.Fatal("pump outlived its closed session")
	}
}

// Background command exits and due scheduled tasks are published on the same
// registry as agent results, so they take the same path to the model.
func TestACPCommandAndScheduleEventsTakeTheSamePath(t *testing.T) {
	var mu sync.Mutex
	var requests [][]string
	s, conn, id := e2eConnection(t, &taskClient{}, func(w http.ResponseWriter, r *http.Request) {
		users, _ := taskRequestUsers(r)
		mu.Lock()
		requests = append(requests, users)
		mu.Unlock()
		writeTaskText(w, "noted")
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	sess := s.lookupSession(id)
	reg := sess.agent.Tasks(string(id))
	reg.Publish(task.Event{ID: "exec-7", Kind: task.KindCommand, AgentType: "exec", Description: "npm test", Seq: 1, Status: task.StatusFailed, Result: "exit status 1"})
	reg.Publish(task.Event{ID: "nightly", Kind: task.KindSchedule, AgentType: "schedule", Description: "every 1h", Seq: 1, Status: task.StatusDone, Result: "check the build"})
	waitTaskCondition(t, "both events to be held", func() bool { return len(heldTaskEvents(sess.tasks)) == 2 })

	taskPrompt(ctx, t, conn, id, "hello")
	mu.Lock()
	defer mu.Unlock()
	delivered := strings.Join(requests[len(requests)-1], "\n")
	for _, want := range []string{"hello", "Background command exec-7", "exit status 1", "<scheduled-task>", "check the build"} {
		if !strings.Contains(delivered, want) {
			t.Fatalf("prompt did not carry %q: %q", want, delivered)
		}
	}
	if held := heldTaskEvents(sess.tasks); len(held) != 0 {
		t.Fatalf("delivered events are still held: %+v", held)
	}
}

func taskTestEvent(id, result string) task.Event {
	return task.Event{ID: id, Seq: 1, AgentType: "explore", Description: "probe " + id, Status: task.StatusDone, Result: result}
}

func TestTaskPumpDeliversHeldEventsAsOneInputOnce(t *testing.T) {
	reg := task.NewRegistry()
	defer reg.Close()
	var mu sync.Mutex
	active := false
	var inputs []code.TurnInput
	p := newTaskPump("session", reg, func(input code.TurnInput) error {
		mu.Lock()
		defer mu.Unlock()
		if !active {
			return code.ErrNoActiveTurn
		}
		inputs = append(inputs, input)
		return nil
	})
	defer p.stop()

	reg.Publish(taskTestEvent("a", "RESULT-A"))
	waitTaskCondition(t, "the first held event", func() bool { return len(heldTaskEvents(p)) == 1 })
	reg.Publish(taskTestEvent("b", "RESULT-B"))
	waitTaskCondition(t, "the second held event", func() bool { return len(heldTaskEvents(p)) == 2 })

	mu.Lock()
	active = true
	mu.Unlock()
	p.flush()
	p.flush()

	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != 1 || len(heldTaskEvents(p)) != 0 {
		t.Fatalf("inputs = %+v, held = %+v", inputs, heldTaskEvents(p))
	}
	input := inputs[0]
	if input.ID != "task-a-1" || input.Intent != code.TurnInputSteer || input.Origin != "task" || len(input.Content) != 1 || !input.Content[0].Hidden {
		t.Fatalf("input = %+v", input)
	}
	text := input.Content[0].Text
	if a, b := strings.Index(text, "RESULT-A"), strings.Index(text, "RESULT-B"); a < 0 || b < a || strings.Count(text, taskNoticeTag) != 2 {
		t.Fatalf("input text = %q", text)
	}
}

func TestTaskPumpCloseReturnsUndeliveredEventsToRegistry(t *testing.T) {
	reg := task.NewRegistry()
	defer reg.Close()
	p := newTaskPump("session", reg, func(code.TurnInput) error { return code.ErrNoActiveTurn })

	reg.Publish(taskTestEvent("a", "RESULT-A"))
	waitTaskCondition(t, "the first held event", func() bool { return len(heldTaskEvents(p)) == 1 })
	taken := p.take()
	reg.Publish(taskTestEvent("b", "RESULT-B"))
	waitTaskCondition(t, "the second held event", func() bool { return len(heldTaskEvents(p)) == 1 })

	p.close()
	p.close()
	if !taskPumpStopped(p) {
		t.Fatal("closed pump is still running")
	}
	// A prompt that fails after its session closed must not lose what it took.
	p.restore(taken)

	var returned []string
	for {
		select {
		case ev := <-reg.Events():
			returned = append(returned, ev.ID)
			continue
		default:
		}
		break
	}
	if strings.Join(returned, ",") != "b,a" {
		t.Fatalf("events returned to the registry = %q, want each undelivered event once", returned)
	}
}

func TestTaskPumpStopsWhenRegistryCloses(t *testing.T) {
	reg := task.NewRegistry()
	p := newTaskPump("session", reg, func(code.TurnInput) error { return nil })
	reg.Close()
	waitTaskCondition(t, "the pump to follow its registry", func() bool { return taskPumpStopped(p) })
}

func TestHoldTaskEventsKeepsResultsAndLatestScheduleTrigger(t *testing.T) {
	schedule := func(seq int) task.Event {
		return task.Event{ID: "nightly", Kind: task.KindSchedule, Seq: seq, Description: "every 1m", Result: "check the build"}
	}
	held := holdTaskEvents(nil, []task.Event{taskTestEvent("a", "RESULT-A"), schedule(1)})
	held = holdTaskEvents(held, []task.Event{schedule(2), taskTestEvent("a", "RESULT-A2"), schedule(3)})

	var got []string
	for _, ev := range held {
		got = append(got, fmt.Sprintf("%s/%s/%d/%s", ev.Kind, ev.ID, ev.Seq, ev.Result))
	}
	want := []string{"/a/1/RESULT-A", "/a/1/RESULT-A2", "schedule/nightly/3/check the build"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("held = %q, want %q", got, want)
	}
}
