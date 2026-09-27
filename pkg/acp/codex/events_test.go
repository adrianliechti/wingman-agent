package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestFileChangeContentAddUnifiedDiff(t *testing.T) {
	raw := []byte(`{"changes":[{"path":"/p/NewFile.kt","kind":{"type":"add"},"diff":"--- /dev/null\n+++ /p/NewFile.kt\n@@ -0,0 +1,3 @@\n+package test\n+\n+class NewFile {}"}]}`)
	content := fileChangeContent(raw)
	if len(content) != 1 || content[0].Diff == nil {
		t.Fatalf("expected one diff block, got %#v", content)
	}
	d := content[0].Diff
	if d.Path != "/p/NewFile.kt" {
		t.Errorf("path = %q", d.Path)
	}
	if d.OldText != nil {
		t.Errorf("add should have nil oldText, got %q", *d.OldText)
	}
	if want := "package test\n\nclass NewFile {}"; d.NewText != want {
		t.Errorf("newText = %q, want %q", d.NewText, want)
	}
}

func TestMessageUpdatesCarryIDsAndPhase(t *testing.T) {
	user := userMessageUpdate(acp.TextBlock("hello"), "user-id")
	if user.UserMessageChunk == nil || user.UserMessageChunk.MessageId == nil || *user.UserMessageChunk.MessageId != "user-id" {
		t.Fatalf("user update = %#v", user)
	}

	agent := agentMessageUpdate("answer", "agent-id", "final_answer")
	if agent.AgentMessageChunk == nil || agent.AgentMessageChunk.MessageId == nil || *agent.AgentMessageChunk.MessageId != "agent-id" {
		t.Fatalf("agent update = %#v", agent)
	}
	b, err := json.Marshal(agent.AgentMessageChunk.Meta)
	if err != nil || string(b) != `{"codex":{"phase":"final_answer"}}` {
		t.Fatalf("agent meta = %s, err=%v", b, err)
	}

	thought := agentThoughtUpdate("thinking", "thought-id")
	if thought.AgentThoughtChunk == nil || thought.AgentThoughtChunk.MessageId == nil || *thought.AgentThoughtChunk.MessageId != "thought-id" {
		t.Fatalf("thought update = %#v", thought)
	}
}

func TestElicitationParamsPreserveSchemaAndURLFields(t *testing.T) {
	var p elicitationParams
	err := json.Unmarshal([]byte(`{"threadId":"t","serverName":"mcp","mode":"form","message":"Choose","requestedSchema":{"type":"object","properties":{"answer":{"type":"string"}}},"url":"https://example.com","elicitationId":"e-1","_meta":{"persist":"session"}}`), &p)
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://example.com" || p.ElicitationID != "e-1" || p.Meta["persist"] != "session" {
		t.Fatalf("params = %#v", p)
	}
	var schema acp.UnstableElicitationSchema
	if err := json.Unmarshal(p.RequestedSchema, &schema); err != nil || schema.Properties["answer"] == nil {
		t.Fatalf("schema = %#v, err=%v", schema, err)
	}
}

func TestFileChangeContentAddRawContent(t *testing.T) {

	raw := []byte(`{"changes":[{"path":"/p/Raw.kt","kind":{"type":"add"},"diff":"fun main() {}\n"}]}`)
	content := fileChangeContent(raw)
	if len(content) != 1 || content[0].Diff == nil {
		t.Fatalf("expected one diff block, got %#v", content)
	}
	if got := content[0].Diff.NewText; got != "fun main() {}\n" {
		t.Errorf("newText = %q", got)
	}
	if content[0].Diff.OldText != nil {
		t.Errorf("add should have nil oldText")
	}
}

func TestFileChangeContentUpdate(t *testing.T) {
	raw := []byte(`{"changes":[{"path":"/p/a.go","kind":{"type":"update"},"diff":"--- a/p/a.go\n+++ b/p/a.go\n@@ -1,3 +1,3 @@\n line one\n-old line\n+new line\n line three"}]}`)
	content := fileChangeContent(raw)
	if len(content) != 1 || content[0].Diff == nil {
		t.Fatalf("expected one diff block, got %#v", content)
	}
	d := content[0].Diff
	if d.OldText == nil || *d.OldText != "line one\nold line\nline three" {
		t.Errorf("oldText = %v", d.OldText)
	}
	if d.NewText != "line one\nnew line\nline three" {
		t.Errorf("newText = %q", d.NewText)
	}
}

func TestFileChangeLocationsAreDistinct(t *testing.T) {
	raw := json.RawMessage(`{"changes":[{"path":"/p/a.go"},{"path":"/p/a.go"},{"path":"/p/b.go"},{"path":""}]}`)
	locations := fileChangeLocations(raw)
	if len(locations) != 2 || locations[0].Path != "/p/a.go" || locations[1].Path != "/p/b.go" {
		t.Fatalf("locations = %#v", locations)
	}
}

func TestCommandActionToolCall(t *testing.T) {
	title, kind, input, locs, ok := commandActionToolCall([]commandAction{{Type: "read", Path: "/x"}})
	if !ok || title != "Read file" || kind != acp.ToolKindRead || input != nil || len(locs) != 1 || locs[0].Path != "/x" {
		t.Errorf("read: got %q %v %v %v", title, kind, locs, ok)
	}

	title, kind, input, locs, ok = commandActionToolCall([]commandAction{{Type: "search", Query: "foo", Path: "/p"}})
	query, _ := input.(map[string]any)["query"].(string)
	if !ok || title != "Search files" || kind != acp.ToolKindSearch || query != "foo" || len(locs) != 1 || locs[0].Path != "/p" {
		t.Errorf("search: got %q %v %v %v", title, kind, locs, ok)
	}

	title, kind, input, locs, ok = commandActionToolCall([]commandAction{{Type: "listFiles", Path: "/p"}})
	if !ok || title != "List files" || kind != acp.ToolKindRead || len(locs) != 1 || locs[0].Path != "/p" {
		t.Errorf("listFiles: got %q %v %v %v", title, kind, locs, ok)
	}
	if title, _, _, _, _ := commandActionToolCall([]commandAction{{Type: "listFiles"}}); title != "List files" {
		t.Errorf("listFiles no path: got %q", title)
	}

	if _, _, _, _, ok := commandActionToolCall([]commandAction{{Type: "read", Path: "/x"}, {Type: "read", Path: "/y"}}); ok {
		t.Errorf("multiple actions should not resolve to a single mapping")
	}
	if _, _, _, _, ok := commandActionToolCall([]commandAction{{Type: "unknown"}}); ok {
		t.Errorf("unknown action should not resolve")
	}
}

func TestDisplayLocationsDoNotBecomeRawInput(t *testing.T) {
	opts := appendDisplayLocations(nil, []acp.ToolCallLocation{{Path: "/project/main.go"}})
	update := acp.StartToolCall("read-1", "Read file", opts...)
	if update.ToolCall == nil {
		t.Fatal("missing tool call")
	}
	if len(update.ToolCall.Locations) != 1 || update.ToolCall.Locations[0].Path != "/project/main.go" {
		t.Fatalf("locations = %#v", update.ToolCall.Locations)
	}
	if update.ToolCall.RawInput != nil {
		t.Fatalf("raw input duplicated the location: %#v", update.ToolCall.RawInput)
	}
}

func TestCommandRawInputOmitsEmptyWorkingDirectory(t *testing.T) {
	input := commandRawInput("go test ./...", "")
	if input["command"] != "go test ./..." {
		t.Fatalf("command = %#v", input["command"])
	}
	if _, ok := input["cwd"]; ok {
		t.Fatalf("empty cwd should be omitted: %#v", input)
	}
}

func TestStripShellPrefix(t *testing.T) {
	cases := map[string]string{
		"/bin/zsh -c npm install":                 "npm install",
		"/bin/bash -lc npm install":               "npm install",
		"zsh npm install":                         "npm install",
		"sh -c ls -la":                            "ls -la",
		"npm install":                             "npm install",
		"/bin/bash -lc './tests.cmd -D=v'":        "./tests.cmd -D=v",
		"/bin/zsh -c 'echo hello'":                "echo hello",
		`"/bin/zsh -lc sed -n '1,20p' README.md"`: `sed -n '1,20p' README.md`,
	}
	for in, want := range cases {
		if got := stripShellPrefix(in); got != want {
			t.Errorf("stripShellPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileChangeContentDeleteMeta(t *testing.T) {
	raw := []byte(`{"changes":[{"path":"/p/gone.go","kind":{"type":"delete"},"diff":"--- a/p/gone.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-line one\n-line two"}]}`)
	content := fileChangeContent(raw)
	if len(content) != 1 || content[0].Diff == nil {
		t.Fatalf("expected one diff block, got %#v", content)
	}
	d := content[0].Diff
	if d.Meta["kind"] != "delete" {
		t.Errorf("meta kind = %v, want delete", d.Meta["kind"])
	}
	if d.NewText != "" {
		t.Errorf("delete newText = %q, want empty", d.NewText)
	}
	if d.OldText == nil || *d.OldText != "line one\nline two" {
		t.Errorf("delete oldText = %v", d.OldText)
	}
}

func TestIsFatalTurnError(t *testing.T) {
	cases := []struct {
		name string
		info string
		want bool
	}{
		{"unauthorized string", `"unauthorized"`, true},
		{"usage limit string", `"usageLimitExceeded"`, true},
		{"rate limit string", `"rateLimitExceeded"`, true},
		{"other string", `"somethingElse"`, false},
		{"http 401 object", `{"httpConnectionFailed":{"httpStatusCode":401}}`, true},
		{"stream disconnected 401", `{"responseStreamDisconnected":{"httpStatusCode":401}}`, true},
		{"http 500 object", `{"httpConnectionFailed":{"httpStatusCode":500}}`, false},
		{"empty", ``, false},
		{"null", `null`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isFatalTurnError([]byte(c.info)); got != c.want {
				t.Errorf("isFatalTurnError(%s) = %v, want %v", c.info, got, c.want)
			}
		})
	}

	d := newEventDispatcher(context.Background(), nil, "session")
	d.handle("error", json.RawMessage(`{"error":{"codexErrorInfo":"rateLimitExceeded"}}`))
	if d.getFailure() == nil {
		t.Fatal("rate-limit notification did not record a turn failure")
	}
	select {
	case <-d.done:
	default:
		t.Fatal("rate-limit notification did not unblock the turn")
	}
}

func TestToolStatusFor(t *testing.T) {
	cases := map[string]acp.ToolCallStatus{
		"inProgress":  acp.ToolCallStatusInProgress,
		"completed":   acp.ToolCallStatusCompleted,
		"failed":      acp.ToolCallStatusFailed,
		"declined":    acp.ToolCallStatusFailed,
		"interrupted": acp.ToolCallStatusFailed,
	}
	for status, want := range cases {
		if got := toolStatusFor(status); got != want {
			t.Errorf("toolStatusFor(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestTokenUsageComponentsMatchTotal(t *testing.T) {
	d := newEventDispatcher(context.Background(), nil, "session")
	d.handleTokenUsage([]byte(`{"tokenUsage":{"last":{"totalTokens":110,"inputTokens":100,"cachedInputTokens":40,"outputTokens":10,"reasoningOutputTokens":5}}}`))
	u := d.getUsage()
	if u == nil || u.CachedReadTokens == nil || u.ThoughtTokens == nil {
		t.Fatalf("usage = %#v", u)
	}
	if got := u.InputTokens + *u.CachedReadTokens + u.OutputTokens + *u.ThoughtTokens; got != u.TotalTokens {
		t.Fatalf("component sum = %d, total = %d", got, u.TotalTokens)
	}
	if u.InputTokens != 60 || *u.CachedReadTokens != 40 || u.OutputTokens != 5 || *u.ThoughtTokens != 5 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestCodexReasoningUsageStaysWithinOutput(t *testing.T) {
	for _, tt := range []struct {
		name                                         string
		output, reasoning, wantOutput, wantReasoning int
	}{
		{"no reasoning", 10, 0, 10, 0},
		{"all reasoning", 10, 10, 0, 10},
		{"excess reasoning", 10, 20, 0, 10},
		{"negative reasoning", 10, -5, 10, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newEventDispatcher(context.Background(), nil, "s")
			payload, _ := json.Marshal(map[string]any{"tokenUsage": map[string]any{"last": map[string]any{
				"totalTokens": tt.output, "outputTokens": tt.output, "reasoningOutputTokens": tt.reasoning,
			}}})
			d.handleTokenUsage(payload)
			u := d.getUsage()
			if u.OutputTokens != tt.wantOutput || *u.ThoughtTokens != tt.wantReasoning {
				t.Fatalf("output=%d reasoning=%d, want %d/%d", u.OutputTokens, *u.ThoughtTokens, tt.wantOutput, tt.wantReasoning)
			}
		})
	}
}

func TestCodexRateLimitNoteOnlySpeaksWhenReached(t *testing.T) {
	// The rolling heartbeat fires several times per turn; an unconstrained
	// snapshot must stay silent.
	quiet := `{"rateLimits":{"limitName":"weekly","primary":{"usedPercent":12}}}`
	if got := rateLimitNote(json.RawMessage(quiet)); got != "" {
		t.Fatalf("unconstrained snapshot produced %q, want silence", got)
	}

	reached := `{"rateLimits":{"limitName":"weekly","rateLimitReachedType":"primary","primary":{"usedPercent":100,"resetsAt":1788000000}}}`
	got := rateLimitNote(json.RawMessage(reached))
	if !strings.Contains(got, "Rate limit reached") || !strings.Contains(got, "weekly") {
		t.Fatalf("reached snapshot = %q", got)
	}

	spend := `{"rateLimits":{"spendControlReached":true}}`
	if got := rateLimitNote(json.RawMessage(spend)); !strings.Contains(got, "Spend limit reached") {
		t.Fatalf("spend snapshot = %q", got)
	}
}

func TestFileChangeDiffStats(t *testing.T) {
	stats := func(t *testing.T, change string) (map[string]any, *acp.ToolCallContentDiff) {
		t.Helper()
		content := fileChangeContent(json.RawMessage(`{"changes":[` + change + `]}`))
		if len(content) != 1 || content[0].Diff == nil {
			t.Fatalf("content = %#v", content)
		}
		d := content[0].Diff
		jetbrains, _ := d.Meta["jetbrains"].(map[string]any)
		air, _ := jetbrains["air"].(map[string]any)
		s, _ := air["diffStats"].(map[string]any)
		return s, d
	}
	for _, tt := range []struct {
		name, change   string
		added, removed int
	}{
		{"update", `{"path":"/p/a.go","kind":{"type":"update"},"diff":"--- a/p/a.go\n+++ b/p/a.go\n@@ -1,3 +1,4 @@\n line one\n-old\n+new\n+extra\n line three\n"}`, 2, 1},
		{"two hunks and marker", `{"path":"/p/a.go","kind":{"type":"update"},"diff":"@@ -1,2 +1,2 @@\n-a\n+b\n c\n@@ -10 +10 @@\n-x\n\\ No newline at end of file\n+y\n\\ No newline at end of file"}`, 2, 2},
		{"add raw", `{"path":"/p/new.txt","kind":{"type":"add"},"diff":"one\r\ntwo\rthree"}`, 3, 0},
		{"add unified", `{"path":"/p/new.txt","kind":{"type":"add"},"diff":"--- /dev/null\n+++ /p/new.txt\n@@ -0,0 +1,2 @@\n+a\n+b"}`, 2, 0},
		{"delete raw", `{"path":"/p/old.txt","kind":{"type":"delete"},"diff":"gone\n\n"}`, 0, 2},
		{"empty add", `{"path":"/p/empty","kind":{"type":"add"},"diff":""}`, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, d := stats(t, tt.change)
			if s == nil || s["version"] != 1 || s["added"] != tt.added || s["removed"] != tt.removed {
				t.Fatalf("diffStats = %#v, want +%d -%d", s, tt.added, tt.removed)
			}
			if d.Meta["kind"] == nil {
				t.Fatalf("diff kind was dropped: %#v", d.Meta)
			}
		})
	}
	for _, change := range []string{
		`{"path":"/p/a.go","kind":{"type":"update"},"diff":"@@ -1,3 +1,3 @@\n-a\n+b"}`,
		`{"path":"/p/a.go","kind":{"type":"update"},"diff":"@@ -5,1 +5,1 @@\n-a\n+b\n@@ -1,1 +1,1 @@\n-c\n+d"}`,
		`{"path":"/p/a.go","kind":{"type":"update"},"diff":"no hunks"}`,
		`{"path":"/p/a.go","kind":{"type":"update"},"diff":"@@ -1 +1 @@\n\\ No newline at end of file\n-a\n+b"}`,
	} {
		if s, d := stats(t, change); s != nil || d.Meta["kind"] != "update" {
			t.Errorf("invalid patch published stats %#v for %s", s, change)
		}
	}
	_, d := stats(t, `{"path":"/p/old.txt","kind":{"type":"delete"},"diff":"gone\n"}`)
	if d.OldText == nil || *d.OldText != "gone\n" || d.NewText != "" {
		t.Fatalf("raw delete content = old %v new %q", d.OldText, d.NewText)
	}
}

func TestToolCallsCarryCodexToolNames(t *testing.T) {
	name := func(meta map[string]any) string { return toolNameFromMeta(meta) }
	for source, want := range map[string]string{"unifiedExecStartup": "exec_command", "unifiedExecInteraction": "write_stdin", "agent": "", "userShell": ""} {
		u, ok := itemToolCallStart(json.RawMessage(`{"command":"ls","source":"`+source+`"}`), "cmd", "commandExecution", acp.ToolCallStatusInProgress)
		if !ok || name(u.ToolCall.Meta) != want {
			t.Errorf("source %s: meta = %#v, want %q", source, u.ToolCall.Meta, want)
		}
	}
	u, _ := itemToolCallStart(json.RawMessage(`{"command":"cat a","source":"unifiedExecStartup","commandActions":[{"type":"read","path":"/a"}]}`), "read", "commandExecution", acp.ToolCallStatusCompleted)
	if name(u.ToolCall.Meta) != "exec_command" || u.ToolCall.Title != "Read file" {
		t.Errorf("command action = %#v", u.ToolCall)
	}
	u, _ = itemToolCallStart(json.RawMessage(`{"tool":"lookup","namespace":"mcp__docs__","arguments":{}}`), "dyn", "dynamicToolCall", acp.ToolCallStatusInProgress)
	if name(u.ToolCall.Meta) != "mcp__docs__lookup" {
		t.Errorf("dynamic tool meta = %#v", u.ToolCall.Meta)
	}
	if u, _ := imageViewToolCall(json.RawMessage(`{"id":"img","path":"/a.png"}`)); name(u.ToolCall.Meta) != "view_image" {
		t.Errorf("image view meta = %#v", u.ToolCall.Meta)
	}
	if tc := permissionsToolCall(permissionsApprovalParams{ItemID: "perm"}); name(tc.Meta) != "request_permissions" {
		t.Errorf("permissions meta = %#v", tc.Meta)
	}

	d := newEventDispatcher(context.Background(), discardingConn(t), "session")
	d.handle("item/started", json.RawMessage(`{"item":{"id":"cmd","type":"commandExecution","command":"ls","source":"unifiedExecStartup"}}`))
	if d.commandName("cmd") != "exec_command" {
		t.Fatalf("started command name = %q", d.commandName("cmd"))
	}
	d.handle("item/completed", json.RawMessage(`{"item":{"id":"cmd","type":"commandExecution","status":"completed","source":"unifiedExecStartup"}}`))
	if d.commandName("cmd") != "" {
		t.Fatal("completed command kept its approval name")
	}
}

func TestAuthenticationErrorsUseACPLoginFlow(t *testing.T) {
	isAuthRequired := func(err error) bool {
		var re *acp.RequestError
		return errors.As(err, &re) && re.Code == acp.NewAuthRequired(nil).Code
	}
	for _, info := range []string{`"unauthorized"`, `{"responseStreamDisconnected":{"httpStatusCode":401}}`, `{"anyFutureVariant":{"httpStatusCode":401}}`} {
		d := newEventDispatcher(context.Background(), nil, "session")
		d.handle("error", json.RawMessage(`{"error":{"message":"Sign in","codexErrorInfo":`+info+`},"willRetry":true}`))
		if d.getFailure() != nil {
			t.Fatalf("%s: retrying auth error failed the turn", info)
		}
		d.handle("error", json.RawMessage(`{"error":{"message":"Sign in","codexErrorInfo":`+info+`},"willRetry":false}`))
		if err := d.getFailure(); !isAuthRequired(err) {
			t.Fatalf("%s: failure = %v, want auth_required", info, err)
		}
		select {
		case <-d.done:
		default:
			t.Fatalf("%s: auth failure did not end the turn", info)
		}
	}

	d := newEventDispatcher(context.Background(), nil, "session")
	d.handle("turn/completed", json.RawMessage(`{"turn":{"id":"t","status":"failed","error":{"message":"Sign in","codexErrorInfo":"unauthorized"}}}`))
	if err := d.getFailure(); !isAuthRequired(err) {
		t.Fatalf("failed turn = %v, want auth_required", err)
	}
	d = newEventDispatcher(context.Background(), nil, "session")
	d.handle("turn/completed", json.RawMessage(`{"turn":{"id":"t","status":"failed","error":{"message":"boom","codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":500}}}}}`))
	if err := d.getFailure(); err == nil || isAuthRequired(err) {
		t.Fatalf("non-auth failure = %v", err)
	}
}

func discardingConn(t *testing.T) *acp.AgentSideConnection {
	t.Helper()
	agentIO, clientIO := net.Pipe()
	t.Cleanup(func() {
		_ = agentIO.Close()
		_ = clientIO.Close()
	})
	_ = acp.NewClientSideConnection(&liveClient{}, clientIO, clientIO)
	return acp.NewAgentSideConnection(&Agent{}, agentIO, agentIO)
}
