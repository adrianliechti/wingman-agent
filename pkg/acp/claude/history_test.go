package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func TestScanSessionMetadataPrefersLatestAITitle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	lines := `{"type":"user","cwd":"/proj","message":{"role":"user","content":"hello"}}
{"type":"ai-title","aiTitle":"First title"}
{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}
{"type":"ai-title","aiTitle":"Second, more accurate title"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	title, cwd := scanSessionMetadata(path)
	if title != "Second, more accurate title" {
		t.Errorf("title = %q, want the latest ai-title line", title)
	}
	if cwd != "/proj" {
		t.Errorf("cwd = %q, want /proj", cwd)
	}
}

func TestScanSessionModelUsesLatestAssistantModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	lines := `{"type":"assistant","message":{"model":"claude-sonnet-5","content":[]}}
{"type":"user","message":{"content":"continue"}}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[]}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := scanSessionModel(path); got != "claude-opus-4-8" {
		t.Fatalf("model = %q", got)
	}
}

func TestScanSessionModelIgnoresSyntheticAndNestedResponses(t *testing.T) {
	for _, tt := range []struct{ name, tail string }{
		{"synthetic", `{"type":"assistant","message":{"model":"<synthetic>"}}`},
		{"placeholder", `{"type":"assistant","message":{"model":"  <unknown>  "}}`},
		{"nested tool", `{"type":"assistant","parent_tool_use_id":"task-1","message":{"model":"claude-haiku-4-5"}}`},
		{"nested agent", `{"type":"assistant","parent_agent_id":"child-1","message":{"model":"claude-haiku-4-5"}}`},
		{"empty model", `{"type":"assistant","message":{"model":"  "}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			for _, prefix := range []string{"", "{\"type\":\"assistant\",\"message\":{\"model\":\" claude-opus-5 \"}}\n"} {
				if err := os.WriteFile(path, []byte(prefix+tt.tail+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				want := ""
				if prefix != "" {
					want = "claude-opus-5"
				}
				if got := scanSessionModel(path); got != want {
					t.Fatalf("model=%q, want %q", got, want)
				}
			}
		})
	}
}

func TestStripMarkerTags(t *testing.T) {

	for _, in := range []string{
		"<command-name>/model</command-name>",
		"<local-command-stderr>err</local-command-stderr>",
		"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>",
	} {
		if _, ok := stripMarkerTags(in); ok {
			t.Errorf("stripMarkerTags(%q) should drop the message", in)
		}
	}

	if got, ok := stripMarkerTags("<local-command-stdout>out</local-command-stdout>"); !ok || got != "out" {
		t.Errorf("stdout payload = %q %v, want kept as %q", got, ok, "out")
	}

	mixed := "<command-name>/model</command-name>" +
		"<local-command-stdout>Set model to opus</local-command-stdout>" +
		"\nplease continue"
	got, ok := stripMarkerTags(mixed)
	if !ok {
		t.Fatalf("expected mixed content to be kept")
	}
	if want := "Set model to opus\nplease continue"; got != want {
		t.Errorf("stripMarkerTags mixed = %q, want %q", got, want)
	}

	if got, ok := stripMarkerTags("hello"); !ok || got != "hello" {
		t.Errorf("plain text = %q %v", got, ok)
	}
}

func TestReplayUserText(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"is that right?<system-reminder>be nice</system-reminder>", "is that right?"},
		{"<system-reminder>do not mention this</system-reminder>", ""},
		{"<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>pkg/acp</command-args>", "/review pkg/acp"},
		{"<command-name>/deploy</command-name>", "/deploy"},
		{"<command-name>/model</command-name>\n<command-args>opus</command-args>", ""},
		{"<command-name>/context</command-name>", ""},
		{"<command-name>/review</command-name><local-command-stdout></local-command-stdout>", ""},
	} {
		got, ok := replayUserText(tt.in)
		if ok != (tt.want != "") || got != tt.want {
			t.Errorf("replayUserText(%q) = %q %v, want %q", tt.in, got, ok, tt.want)
		}
	}
}

func TestReplayHidesInjectedAndLoginText(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	wire := &bytes.Buffer{}
	conn := acp.NewAgentSideConnection(New(Options{Stderr: io.Discard}), wire, reader)
	history := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"is that right?<system-reminder>Do not mention this reminder.</system-reminder>"}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<command-name>/review</command-name><command-args>now</command-args>"}]}}`,
		`{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text","text":"Not logged in · Please run /login"}]}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-5","content":[{"type":"text","text":"yes"}]}}`,
	}, "\n")
	if err := streamHistory(context.Background(), conn, "s", t.TempDir(), strings.NewReader(history)); err != nil {
		t.Fatal(err)
	}
	var user []string
	updates := readTranscriptUpdates(t, wire.Bytes())
	for _, u := range updates {
		if u.UserMessageChunk != nil && u.UserMessageChunk.Content.Text != nil {
			user = append(user, u.UserMessageChunk.Content.Text.Text)
		}
	}
	if !slices.Equal(user, []string{"is that right?", "/review now"}) {
		t.Fatalf("user chunks = %q", user)
	}
	if got := transcriptText(updates); got != "yes" {
		t.Fatalf("agent text = %q, want login text hidden", got)
	}
}

func TestTranscriptPathFollowsClaudeProjectNaming(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	if got := encodeCwd("/a/ü😀b"); got != "-a----b" {
		t.Fatalf("encodeCwd = %q, want one dash per UTF-16 unit", got)
	}

	cwd := filepath.Join(t.TempDir(), filepath.FromSlash(strings.Repeat("long-segment/", 20)))
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := pathutil.Resolve(cwd)
	if err != nil {
		t.Fatal(err)
	}
	key := encodeCwd(resolved)
	dir := filepath.Join(config, "projects", key[:maxProjectKeyLen]+"-1x2y3z")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHistoryTranscript(t, filepath.Join(dir, "s.jsonl"), cwd, "hello")
	if got := transcriptPath(cwd, "s", nil); got != filepath.Join(dir, "s.jsonl") {
		t.Fatalf("transcriptPath = %q, want the hashed project directory under CLAUDE_CONFIG_DIR", got)
	}
	if got := transcriptPath(cwd, "missing", nil); got != "" {
		t.Fatalf("transcriptPath of a missing transcript = %q", got)
	}
}

func writeHistoryTranscript(t *testing.T, path, cwd, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{
		"type": "user", "cwd": cwd,
		"message": map[string]string{"role": "user", "content": text},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryUsesSubprocessEnvironment(t *testing.T) {
	home := testenv.UserHome(t)
	parentConfig, childConfig, childHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", parentConfig)
	cwd, err := pathutil.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		env    []string
		config string
	}{
		{"inherited", nil, parentConfig},
		{"overridden", []string{"CLAUDE_CONFIG_DIR=" + childConfig}, childConfig},
		{"cleared", []string{"CLAUDE_CONFIG_DIR=", "HOME=" + home, "USERPROFILE=" + home}, filepath.Join(home, ".claude")},
		{"omitted", []string{"HOME=" + home, "USERPROFILE=" + home}, filepath.Join(home, ".claude")},
		{"child home", []string{"HOME=" + childHome, "USERPROFILE=" + childHome}, filepath.Join(childHome, ".claude")},
		{"last value wins", []string{"CLAUDE_CONFIG_DIR=" + parentConfig, "CLAUDE_CONFIG_DIR=" + childConfig}, childConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := acp.SessionId(uuid.NewString())
			otherConfig := parentConfig
			if tc.config == parentConfig {
				otherConfig = childConfig
			}
			path := filepath.Join(tc.config, "projects", encodeCwd(cwd), string(id)+".jsonl")
			otherPath := filepath.Join(otherConfig, "projects", encodeCwd(cwd), string(id)+".jsonl")
			writeHistoryTranscript(t, path, cwd, "expected profile")
			writeHistoryTranscript(t, otherPath, cwd, "other profile")

			a := New(Options{Env: tc.env, Stderr: io.Discard})
			a.modelsLoaded = true
			t.Cleanup(func() { _ = a.Close() })
			reader, writer := io.Pipe()
			t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
			wire := &bytes.Buffer{}
			a.SetAgentConnection(acp.NewAgentSideConnection(a, wire, reader))
			ctx := context.Background()

			listed, err := a.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd})
			if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].Title == nil || *listed.Sessions[0].Title != "expected profile" {
				t.Fatalf("list = %+v, %v", listed, err)
			}
			if _, err := a.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: id, Cwd: cwd}); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if _, err := a.LoadSession(ctx, acp.LoadSessionRequest{SessionId: id, Cwd: cwd}); err != nil {
				t.Fatalf("load: %v", err)
			}
			a.lookup(id).pushTitleUpdate(ctx)
			var replayed, title string
			for _, update := range readTranscriptUpdates(t, wire.Bytes()) {
				if chunk := update.UserMessageChunk; chunk != nil && chunk.Content.Text != nil {
					replayed += chunk.Content.Text.Text
				}
				if info := update.SessionInfoUpdate; info != nil && info.Title != nil {
					title = *info.Title
				}
			}
			if replayed != "expected profile" || title != "expected profile" {
				t.Fatalf("replay = %q, title = %q", replayed, title)
			}
			if _, err := a.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: id}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("selected profile transcript survived deletion: %v", err)
			}
			if _, err := os.Stat(otherPath); err != nil {
				t.Fatalf("other profile transcript was affected: %v", err)
			}
			// The next case reuses the profile roots.
			if err := os.Remove(otherPath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLongProjectPrefixesKeepHistoriesSeparate(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	shared := filepath.Join(t.TempDir(), filepath.FromSlash(strings.Repeat("long-segment/", 20)))
	cwds := []string{filepath.Join(shared, "a"), filepath.Join(shared, "b")}
	ids := []string{uuid.NewString(), uuid.NewString()}
	paths := make([]string, len(cwds))
	for i := range cwds {
		if err := os.MkdirAll(cwds[i], 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		cwds[i], err = pathutil.Resolve(cwds[i])
		if err != nil {
			t.Fatal(err)
		}
		key := encodeCwd(cwds[i])
		dir := filepath.Join(config, "projects", key[:maxProjectKeyLen]+"-hash"+filepath.Base(cwds[i]))
		paths[i] = filepath.Join(dir, ids[i]+".jsonl")
		writeHistoryTranscript(t, paths[i], cwds[i], "hello")
		writeHistoryTranscript(t, filepath.Join(dir, "unknown.jsonl"), "", "no recorded cwd")
	}
	for i, cwd := range cwds {
		listed, err := listProjectSessions(cwd, nil)
		if err != nil || len(listed) != 1 || string(listed[0].SessionId) != ids[i] {
			t.Fatalf("list %s = %+v, %v", cwd, listed, err)
		}
		if got := transcriptPath(cwd, ids[i], nil); got != paths[i] {
			t.Fatalf("own transcript = %q, want %q", got, paths[i])
		}
		for _, id := range []string{ids[1-i], "unknown"} {
			if got := transcriptPath(cwd, id, nil); got != "" {
				t.Fatalf("transcript %s from another or unknown project = %q", id, got)
			}
		}
	}
	if all, err := listProjectSessions("", nil); err != nil || len(all) != 2 {
		t.Fatalf("unfiltered list = %+v, %v", all, err)
	}
}
