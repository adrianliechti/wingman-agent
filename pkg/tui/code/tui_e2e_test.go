//go:build e2e

package code

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	coder "github.com/adrianliechti/wingman-agent/pkg/code/agent"
)

func tuiModelServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-5.4","object":"model"}]}`)
		case "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"E2E reply\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens\":2}}}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
}

func waitForTUI(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for TUI state")
}

func simulationText(screen tcell.SimulationScreen) string {
	cells, width, height := screen.GetContents()
	var out strings.Builder
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			cell := cells[y*width+x]
			if len(cell.Runes) == 0 {
				out.WriteByte(' ')
			} else {
				out.WriteRune(cell.Runes[0])
			}
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func postText(t *testing.T, screen tcell.SimulationScreen, value string) {
	t.Helper()
	post := func(event *tcell.EventKey) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := screen.PostEvent(event)
			if err == nil {
				return
			}
			if err != tcell.ErrEventQFull || time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
	}
	for _, r := range value {
		post(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	post(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
}

type tuiE2EHarness struct {
	agent     *coder.Agent
	sessionID string
	screen    tcell.SimulationScreen
}

func newTUIE2EHarness(t *testing.T) *tuiE2EHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	workspace, err := code.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := agent.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	codeAgent := coder.New(workspace, cfg, nil)
	sessionID, err := codeAgent.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	app := New(ctx, codeAgent, sessionID)
	codeAgent.SetUI(app)
	screen := tcell.NewSimulationScreen("UTF-8")
	app.app.SetScreen(screen)
	screen.SetSize(100, 35)

	runDone := make(chan error, 1)
	go func() { runDone <- app.Run() }()
	t.Cleanup(func() {
		app.turns.SetHandler(nil)
		app.turns.Close()
		_ = codeAgent.Close()
		app.app.Stop()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("TUI run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("TUI did not stop")
		}
		workspace.Close()
		cancel()
	})

	waitForTUI(t, func() bool { return app.getPhase() == PhaseIdle && app.input != nil })
	return &tuiE2EHarness{agent: codeAgent, sessionID: sessionID, screen: screen}
}

func TestTUIE2ESendsAndRendersTurn(t *testing.T) {
	model := tuiModelServer(t)
	defer model.Close()
	t.Setenv("WINGMAN_URL", model.URL)
	t.Setenv("WINGMAN_MODEL", "gpt-5.4")
	t.Setenv("WINGMAN_CALLER", "e2e")

	h := newTUIE2EHarness(t)
	postText(t, h.screen, "hello e2e")

	waitForTUI(t, func() bool {
		messages := h.agent.Messages(h.sessionID)
		return len(messages) >= 2 && messages[len(messages)-1].Role == agent.RoleAssistant
	})
	waitForTUI(t, func() bool { return strings.Contains(simulationText(h.screen), "E2E reply") })

	messages := h.agent.Messages(h.sessionID)
	if messages[0].Role != agent.RoleUser || messages[0].Content[0].Text != "hello e2e" {
		t.Fatalf("user message = %+v", messages[0])
	}
}

type tuiSteeringModel struct {
	requests atomic.Int32
	release  chan struct{}
}

func (m *tuiSteeringModel) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-5.4","object":"model"}]}`)
	case "/v1/responses":
		w.Header().Set("Content-Type", "text/event-stream")
		if m.requests.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"Working\"}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-m.release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"Working\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_2\",\"output_index\":0,\"content_index\":0,\"delta\":\"Steering applied\"}\n\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"output\":[{\"type\":\"message\",\"id\":\"msg_2\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"Steering applied\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":4,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens\":2}}}\n\ndata: [DONE]\n\n")
	default:
		http.NotFound(w, r)
	}
}

func TestTUIE2ESteersActiveTurn(t *testing.T) {
	model := &tuiSteeringModel{release: make(chan struct{})}
	modelServer := httptest.NewServer(http.HandlerFunc(model.handler))
	defer modelServer.Close()
	t.Setenv("WINGMAN_URL", modelServer.URL)
	t.Setenv("WINGMAN_MODEL", "gpt-5.4")
	t.Setenv("WINGMAN_CALLER", "e2e")

	h := newTUIE2EHarness(t)
	postText(t, h.screen, "initial request")
	waitForTUI(t, func() bool { return strings.Contains(simulationText(h.screen), "Working") })

	postText(t, h.screen, "steer this turn")
	waitForTUI(t, func() bool { return strings.Contains(simulationText(h.screen), "steer this turn") })
	close(model.release)

	waitForTUI(t, func() bool {
		messages := h.agent.Messages(h.sessionID)
		return len(messages) >= 4 && messages[len(messages)-1].Role == agent.RoleAssistant
	})
	waitForTUI(t, func() bool { return strings.Contains(simulationText(h.screen), "Steering applied") })

	messages := h.agent.Messages(h.sessionID)
	want := []struct {
		role agent.MessageRole
		text string
	}{
		{agent.RoleUser, "initial request"},
		{agent.RoleAssistant, "Working"},
		{agent.RoleUser, "steer this turn"},
		{agent.RoleAssistant, "Steering applied"},
	}
	if len(messages) != len(want) {
		t.Fatalf("messages = %+v", messages)
	}
	for i, expected := range want {
		if messages[i].Role != expected.role || len(messages[i].Content) == 0 || messages[i].Content[0].Text != expected.text {
			t.Fatalf("message %d = %+v, want %s %q", i, messages[i], expected.role, expected.text)
		}
	}
}
