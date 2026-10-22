package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/acp/internal/acptest"
	"github.com/coder/acp-go-sdk"
)

type recordingClient struct {
	liveClient
	mu      sync.Mutex
	updates []acp.SessionNotification
}

func (c *recordingClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, n)
	return nil
}

func (c *recordingClient) titles() []*acp.SessionSessionInfoUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*acp.SessionSessionInfoUpdate
	for _, n := range c.updates {
		if n.Update.SessionInfoUpdate != nil {
			out = append(out, n.Update.SessionInfoUpdate)
		}
	}
	return out
}

func (c *recordingClient) last() acp.SessionUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.updates) == 0 {
		return acp.SessionUpdate{}
	}
	return c.updates[len(c.updates)-1].Update
}

func connectClient(t *testing.T, a *Agent) (*acp.ClientSideConnection, *recordingClient) {
	t.Helper()
	agentSide, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = agentSide.Close()
		_ = clientSide.Close()
	})
	a.SetAgentConnection(acp.NewAgentSideConnection(a, agentSide, agentSide))
	client := &recordingClient{}
	return acp.NewClientSideConnection(client, clientSide, clientSide), client
}

type piRequest struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

func fakePiProcess(t *testing.T, serve func(req piRequest, send func(any))) *process {
	t.Helper()
	local, backend := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = backend.Close()
	})
	p := &process{stdin: local, done: make(chan struct{}), pending: map[string]chan rpcResponse{}}
	go p.readLoop(local)
	go func() {
		scanner := bufio.NewScanner(backend)
		enc := json.NewEncoder(backend)
		for scanner.Scan() {
			var req piRequest
			if json.Unmarshal(scanner.Bytes(), &req) == nil {
				serve(req, func(v any) { _ = enc.Encode(v) })
			}
		}
	}()
	return p
}

func piOK(id string, data any) map[string]any {
	return map[string]any{"type": "response", "id": id, "success": true, "data": data}
}

func titleValues(updates []*acp.SessionSessionInfoUpdate) []string {
	var out []string
	for _, u := range updates {
		if u.Title != nil {
			out = append(out, *u.Title)
		}
	}
	return out
}

func TestFirstPromptNamesSession(t *testing.T) {
	cases := []struct {
		name       string
		state      map[string]any
		renameFail bool
		wantNames  []string
		wantTitles []string
	}{
		{name: "unnamed", state: map[string]any{}, wantNames: []string{"Fix thread 🚀 titles"}, wantTitles: []string{"Fix thread 🚀 titles"}},
		{name: "named", state: map[string]any{"sessionName": " Manual title "}, wantTitles: []string{"Manual title"}},
		{name: "rename fails", state: map[string]any{}, renameFail: true, wantNames: []string{"Fix thread 🚀 titles"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var names []string
			proc := fakePiProcess(t, func(req piRequest, send func(any)) {
				switch req.Type {
				case "get_state":
					send(piOK(req.ID, tc.state))
				case "set_session_name":
					mu.Lock()
					names = append(names, req.Name)
					mu.Unlock()
					if tc.renameFail {
						send(map[string]any{"type": "response", "id": req.ID, "success": false, "error": "unsupported"})
						return
					}
					send(map[string]any{"type": "session_info_changed", "name": req.Name})
					send(piOK(req.ID, nil))
				case "prompt":
					send(piOK(req.ID, nil))
					send(map[string]any{"type": "agent_settled"})
				default:
					send(map[string]any{"type": "response", "id": req.ID, "success": false, "error": "unsupported"})
				}
			})

			a := New(Options{})
			s := newSession("s", t.TempDir(), proc)
			s.autoTitle = true
			a.sessions[s.id] = s
			cc, client := connectClient(t, a)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			prompt := []acp.ContentBlock{acp.TextBlock("  Fix   thread\n🚀"), acp.ImageBlock("AA==", "image/png"), acp.TextBlock("titles ")}
			for _, p := range [][]acp.ContentBlock{prompt, {acp.TextBlock("Second prompt")}} {
				resp, err := cc.Prompt(ctx, acp.PromptRequest{SessionId: s.id, Prompt: p})
				if err != nil || resp.StopReason != acp.StopReasonEndTurn {
					t.Fatalf("prompt = %+v, %v", resp, err)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if strings.Join(names, "|") != strings.Join(tc.wantNames, "|") {
				t.Fatalf("set_session_name calls = %q, want %q", names, tc.wantNames)
			}
			titles := client.titles()
			if got := titleValues(titles); strings.Join(got, "|") != strings.Join(tc.wantTitles, "|") {
				t.Fatalf("titles = %q, want %q", got, tc.wantTitles)
			}
			for _, u := range titles {
				if u.UpdatedAt == nil {
					t.Fatal("live title change carried no updatedAt")
				}
			}
		})
	}
}

func TestSessionInfoChangedForwardsValidNames(t *testing.T) {
	proc := fakePiProcess(t, func(req piRequest, send func(any)) {
		switch req.Type {
		case "prompt":
			send(piOK(req.ID, nil))
			for _, event := range []string{
				`{"type":"session_info_changed","name":"Extension title"}`,
				`{"type":"session_info_changed","name":"Extension title"}`,
				`{"type":"session_info_changed","name":123}`,
				`{"type":"session_info_changed","name":{"text":"invalid"}}`,
				`{"type":"session_info_changed"}`,
				`{"type":"session_info_changed"}`,
			} {
				send(json.RawMessage(event))
			}
			send(map[string]any{"type": "agent_settled"})
		default:
			send(map[string]any{"type": "response", "id": req.ID, "success": false, "error": "unsupported"})
		}
	})

	a := New(Options{})
	s := newSession("s", t.TempDir(), proc)
	s.promptTitle = "First prompt"
	a.sessions[s.id] = s
	cc, client := connectClient(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := cc.Prompt(ctx, acp.PromptRequest{SessionId: s.id, Prompt: []acp.ContentBlock{acp.TextBlock("go")}}); err != nil {
		t.Fatal(err)
	}
	// A cleared name falls back to the first prompt, as Pi lists it.
	if got := titleValues(client.titles()); strings.Join(got, "|") != "Extension title|First prompt" {
		t.Fatalf("titles = %q", got)
	}
}

func TestTurnReportsContextUsageBeforeResponse(t *testing.T) {
	proc := fakePiProcess(t, func(req piRequest, send func(any)) {
		switch req.Type {
		case "prompt":
			send(piOK(req.ID, nil))
			send(map[string]any{"type": "agent_settled"})
		case "get_session_stats":
			send(piOK(req.ID, map[string]any{"cost": 0.25, "contextUsage": map[string]any{"tokens": 1200, "contextWindow": 200000}}))
		default:
			send(map[string]any{"type": "response", "id": req.ID, "success": false, "error": "unsupported"})
		}
	})

	a := New(Options{})
	s := newSession("s", t.TempDir(), proc)
	a.sessions[s.id] = s
	cc, client := connectClient(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := cc.Prompt(ctx, acp.PromptRequest{SessionId: s.id, Prompt: []acp.ContentBlock{acp.TextBlock("go")}}); err != nil {
		t.Fatal(err)
	}
	usage := client.last().UsageUpdate
	if usage == nil || usage.Used != 1200 || usage.Size != 200000 || usage.Cost == nil || usage.Cost.Amount != 0.25 || usage.Cost.Currency != "USD" {
		t.Fatalf("last update before the response = %+v", client.last())
	}
}

func TestContextUsageWaitIsBounded(t *testing.T) {
	proc := fakePiProcess(t, func(req piRequest, send func(any)) {})
	start := time.Now()
	if _, err := proc.getSessionStats(context.Background(), 50*time.Millisecond); err == nil {
		t.Fatal("unanswered stats request succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stats wait took %s", elapsed)
	}
	if err := proc.writeLine(map[string]any{"type": "prompt"}); err != nil {
		t.Fatalf("timed-out stats wait retired the transport: %v", err)
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if len(proc.pending) != 0 {
		t.Fatalf("leaked %d pending requests", len(proc.pending))
	}
}

func TestParseContextUsage(t *testing.T) {
	cases := map[string]bool{
		`{"contextUsage":{"tokens":10,"contextWindow":100}}`:   true,
		`{"contextUsage":{"tokens":0,"contextWindow":100}}`:    true,
		`{"contextUsage":{"tokens":null,"contextWindow":100}}`: false,
		`{"contextUsage":{"tokens":10.5,"contextWindow":100}}`: false,
		`{"contextUsage":{"tokens":-1,"contextWindow":100}}`:   false,
		`{"contextUsage":{"tokens":10,"contextWindow":0}}`:     false,
		`{"contextUsage":null}`:                                false,
		`{"totalMessages":3}`:                                  false,
	}
	for data, want := range cases {
		u := parseContextUsage(json.RawMessage(data))
		if (u != nil) != want {
			t.Errorf("parseContextUsage(%s) = %+v, want update=%v", data, u, want)
		}
		if u != nil && u.UsageUpdate.Cost != nil {
			t.Errorf("zero cost reported for %s", data)
		}
	}
}

func TestPromptTitle(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("  Original \n"),
		acp.ImageBlock("AA==", "image/png"),
		acp.ResourceLinkBlock("ctx", "file:///ctx.txt"),
		acp.TextBlock(strings.Repeat("🚀", 90)),
	}
	if got, want := promptTitle(blocks), "Original "+strings.Repeat("🚀", 71); got != want {
		t.Fatalf("promptTitle = %q, want %q", got, want)
	}
	if got := promptTitle([]acp.ContentBlock{acp.TextBlock(" \n\t")}); got != "" {
		t.Fatalf("blank prompt title = %q", got)
	}
}

func writeSessionFile(t *testing.T, path string, entries []any) {
	t.Helper()
	var b strings.Builder
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func titledHistory(id, cwd, state string) []any {
	entries := []any{
		map[string]any{"type": "session", "id": id, "cwd": cwd},
		map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": "  \n"}},
		map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "data": "AA=="}}}},
		map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "  Original \n"},
			map[string]any{"type": "image", "data": "AA=="},
			map[string]any{"type": "text", "text": strings.Repeat("🚀", 90)},
		}}},
	}
	if state != "fallback" {
		entries = append(entries, map[string]any{"type": "session_info", "name": "Saved title"})
	}
	if state == "cleared" {
		entries = append(entries, map[string]any{"type": "session_info", "name": ""})
	}
	// Push the early entries out of the tail window.
	for range 2100 {
		entries = append(entries, map[string]any{
			"type": "message", "timestamp": "2026-01-01T00:00:00.000Z",
			"message": map[string]any{"role": "assistant", "content": strings.Repeat("x", 200)},
		})
	}
	if state == "cleared-in-tail" {
		entries = append(entries, map[string]any{"type": "session_info", "name": ""})
	}
	return entries
}

func TestSessionFileTitles(t *testing.T) {
	fallback := "Original " + strings.Repeat("🚀", 71)
	for state, want := range map[string]string{
		"saved":           "Saved title",
		"fallback":        fallback,
		"cleared":         fallback,
		"cleared-in-tail": fallback,
	} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "history.jsonl")
			writeSessionFile(t, path, titledHistory("restored", dir, state))

			files := listSessionFiles(dir)
			if len(files) != 1 || files[0].Title != want {
				t.Fatalf("listed sessions = %+v, want title %q", files, want)
			}
			name, prompt := readSessionTitle(path)
			if prompt != fallback || (name != "" && name != want) {
				t.Fatalf("readSessionTitle = %q, %q", name, prompt)
			}
		})
	}
}

func TestLoadSessionRestoresTitleWithoutActivity(t *testing.T) {
	script, dir, env := acptest.CommandHelper(t, "TestPiContractHelper", "PI_CONTRACT_HELPER")
	sessionsDir := t.TempDir()
	writeSessionFile(t, filepath.Join(sessionsDir, "history.jsonl"), titledHistory(contractSessionID, dir, "saved"))

	a := New(Options{Path: script, Env: env, SessionsDir: sessionsDir})
	defer a.Close()
	cc, client := connectClient(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := cc.LoadSession(ctx, acp.LoadSessionRequest{SessionId: contractSessionID, Cwd: dir, McpServers: []acp.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	titles := client.titles()
	if len(titles) != 1 || titles[0].Title == nil || *titles[0].Title != "Saved title" || titles[0].UpdatedAt != nil {
		t.Fatalf("restored titles = %+v", titles)
	}
	if usage := client.last().UsageUpdate; usage == nil || usage.Used != 42 || usage.Size != 1000 {
		t.Fatalf("load did not report context usage: %+v", client.last())
	}
}
