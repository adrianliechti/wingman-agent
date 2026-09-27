package claude

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
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
