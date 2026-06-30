package pi

import (
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestParseAvailableModels(t *testing.T) {
	data := json.RawMessage(`{"models":[
		{"provider":"wingman","id":"claude-sonnet-5","name":"Claude Sonnet 5"},
		{"provider":"wingman","id":"gpt-5.5"},
		{"provider":"","id":"skip"},
		{"provider":"x","id":""}
	]}`)

	got := parseAvailableModels(data)
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d (%+v)", len(got), got)
	}
	if got[0].ID != "wingman/claude-sonnet-5" || got[0].Name != "wingman/Claude Sonnet 5" {
		t.Errorf("model[0] = %+v", got[0])
	}
	if got[1].ID != "wingman/gpt-5.5" || got[1].Name != "wingman/gpt-5.5" {
		t.Errorf("model[1] = %+v (name should fall back to id)", got[1])
	}
}

func TestParseState(t *testing.T) {
	s := parseState(json.RawMessage(`{"sessionId":"abc","thinkingLevel":"high","model":{"provider":"wingman","id":"gpt-5.5"}}`))
	if s.SessionID != "abc" {
		t.Errorf("sessionId = %q", s.SessionID)
	}
	if s.currentModel() != "wingman/gpt-5.5" {
		t.Errorf("currentModel = %q", s.currentModel())
	}
	if s.thinking() != "high" {
		t.Errorf("thinking = %q", s.thinking())
	}

	empty := parseState(json.RawMessage(`{}`))
	if empty.currentModel() != "" {
		t.Errorf("empty currentModel = %q", empty.currentModel())
	}
	if empty.thinking() != defaultThinkingLevel {
		t.Errorf("empty thinking = %q, want default", empty.thinking())
	}
}

func TestToolResultToText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"diff wins", `{"content":[{"type":"text","text":"ok"}],"details":{"diff":"--- a\n+++ b"}}`, "--- a\n+++ b"},
		{"content text", `{"content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}`, "hello world"},
		{"bash stdout", `{"details":{"stdout":"line1","exitCode":0}}`, "line1\n\nexit code: 0"},
		{"stderr", `{"details":{"stderr":"boom","exitCode":1}}`, "stderr:\nboom\n\nexit code: 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toolResultToText(json.RawMessage(c.in)); got != c.want {
				t.Errorf("toolResultToText(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	if got := toolResultToText(nil); got != "" {
		t.Errorf("nil result = %q, want empty", got)
	}
}

func TestPromptToPi(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("hello "),
		acp.ImageBlock("BASE64DATA", "image/png"),
		acp.TextBlock("world"),
	}

	msg, images := promptToPi(blocks)
	if msg != "hello world" {
		t.Errorf("message = %q", msg)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	if images[0].Data != "BASE64DATA" || images[0].MimeType != "image/png" || images[0].Type != "image" {
		t.Errorf("image = %+v", images[0])
	}
}

func TestToolKind(t *testing.T) {
	cases := map[string]acp.ToolKind{
		"read":    acp.ToolKindRead,
		"edit":    acp.ToolKindEdit,
		"write":   acp.ToolKindEdit,
		"bash":    acp.ToolKindExecute,
		"unknown": acp.ToolKindOther,
	}
	for name, want := range cases {
		if got := toolKind(name); got != want {
			t.Errorf("toolKind(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestBuildConfigOptions(t *testing.T) {
	models := []modelEntry{{ID: "wingman/a", Name: "wingman/a"}}
	opts := buildConfigOptions(models, "wingman/a", "high")
	if len(opts) != 2 {
		t.Fatalf("expected model+effort options, got %d", len(opts))
	}
	if opts[0].Select == nil || opts[0].Select.Id != modelConfigID {
		t.Errorf("opts[0] not model select: %+v", opts[0])
	}
	if opts[1].Select == nil || opts[1].Select.Id != effortConfigID {
		t.Errorf("opts[1] not effort select: %+v", opts[1])
	}
	if string(opts[1].Select.CurrentValue) != "high" {
		t.Errorf("effort current = %q, want high", opts[1].Select.CurrentValue)
	}

	// No models → only effort option.
	if only := buildConfigOptions(nil, "", "medium"); len(only) != 1 || only[0].Select.Id != effortConfigID {
		t.Errorf("no-models config = %+v", only)
	}
}
