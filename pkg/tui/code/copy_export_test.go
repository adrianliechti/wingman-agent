package code

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestCopyPickerCopiesSelectedBlockFromSource(t *testing.T) {
	const response = "An example:\n\n```go\n\tprintln(\"hello\")  \n```\n\n> useful quote\n"
	var copied string
	a := &App{
		agent:          newUITestAgent([]agent.Message{{Role: agent.RoleAssistant, Content: []agent.Content{{Text: response}}}}),
		clipboardWrite: func(text string) error { copied = text; return nil },
	}
	a.showCopyPicker()
	if a.popup == nil || len(a.popup.items) != 3 {
		t.Fatal("copy menu did not expose response, code, and quote")
	}
	a.handleKey(inline.KeyEvent{Key: inline.KeyDown})
	a.handleKey(inline.KeyEvent{Key: inline.KeyEnter})
	if copied != "\tprintln(\"hello\")  \n" || a.popup != nil {
		t.Fatalf("selected copy = %q; popup = %v", copied, a.popup)
	}
	if command := a.findBuiltin("/export \"my conversation.md\""); command == nil || command.Name != "/export" {
		t.Fatal("export command did not accept a file path")
	}
	if a.findBuiltin("/exported.md") != nil {
		t.Fatal("export command captured an unrelated prompt")
	}
}

func TestTranscriptExportIncludesVisibleSourceAndOmitsHiddenContext(t *testing.T) {
	messages := []agent.Message{
		{Role: agent.RoleSystem, Content: []agent.Content{{Text: "system secret"}}},
		{Role: agent.RoleUser, Hidden: true, Content: []agent.Content{{Text: "hidden message"}}},
		{Role: agent.RoleUser, Content: []agent.Content{{Text: "question"}, {Text: "hidden context", Hidden: true}, {File: &agent.File{Name: "image.png", Data: "base64 secret"}}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{
			{Reasoning: &agent.Reasoning{Summary: "Checking the code", Content: "encrypted secret"}},
			{Text: "**Original** Markdown\n\n```go\nmain()\n```"},
			{ToolCall: &agent.ToolCall{ID: "call1", Name: "shell", Args: "{}"}},
		}},
		{Role: agent.RoleUser, Content: []agent.Content{
			{ToolResult: &agent.ToolResult{ID: "call1", Name: "shell", Args: "{}", Content: "```inside output\n\n"}},
			{ToolResult: &agent.ToolResult{Name: "hidden_tool", Content: "hidden tool secret"}},
		}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Refusal: "Cannot do that."}}},
	}
	document := transcriptMarkdown(messages, func(name string) bool { return name == "hidden_tool" })
	for _, want := range []string{"## User", "question", "Attachment: image.png", "## Assistant", "Checking the code", "**Original** Markdown", "### Tool: shell", "````text\n```inside output\n\n````", "Cannot do that."} {
		if !strings.Contains(document, want) {
			t.Errorf("export omitted %q: %q", want, document)
		}
	}
	for _, secret := range []string{"secret", "hidden message", "hidden context", "hidden_tool"} {
		if strings.Contains(document, secret) {
			t.Errorf("export included hidden content %q", secret)
		}
	}
	if strings.Count(document, "### Tool: shell") != 1 {
		t.Fatal("tool call and result were exported twice")
	}
	if transcriptMarkdown(messages[:2], nil) != "" {
		t.Fatal("hidden-only transcript was not empty")
	}
}

func TestTranscriptFileExportPreservesExistingFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation with spaces.md")
	if err := writeTranscriptFile(path, "first document"); err != nil {
		t.Fatal(err)
	}
	if err := writeTranscriptFile(path, "replacement"); !os.IsExist(err) {
		t.Fatalf("overwriting existing file returned %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "first document" {
		t.Fatalf("existing export changed: %q, %v", data, err)
	}
}
