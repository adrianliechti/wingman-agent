package claw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
	"github.com/adrianliechti/wingman-agent/pkg/claw/memory"
)

func newTestClaw(t *testing.T) *Claw {
	t.Helper()

	store, err := memory.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.EnsureAgent("main"); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}

	return New(&Config{Memory: store})
}

func TestBuildInstructionsRoleVariants(t *testing.T) {
	c := newTestClaw(t)
	if err := c.config.Memory.EnsureAgent("worker"); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}

	main := c.buildInstructions("main")
	if !strings.Contains(main, "You are the main agent") {
		t.Error("main instructions missing the main-agent section")
	}
	if strings.Contains(main, "# Your Role") {
		t.Error("main instructions contain the sub-agent section")
	}

	worker := c.buildInstructions("worker")
	if strings.Contains(worker, "You are the main agent") {
		t.Error("worker instructions claim to be the main agent")
	}
	if !strings.Contains(worker, "# Your Role") {
		t.Error("worker instructions missing the sub-agent section")
	}
}

func TestBuildInstructionsLabelsGuidelines(t *testing.T) {
	c := newTestClaw(t)

	globalPath := filepath.Join(c.config.Memory.Dir(), "global", "AGENTS.md")
	if err := os.WriteFile(globalPath, []byte("shared rule"), 0644); err != nil {
		t.Fatalf("write global AGENTS.md: %v", err)
	}
	if err := c.config.Memory.WriteAgent("main", "local rule"); err != nil {
		t.Fatalf("WriteAgent: %v", err)
	}

	out := c.buildInstructions("main")

	shared := strings.Index(out, "# Shared Guidelines (global/AGENTS.md)\n\nshared rule")
	local := strings.Index(out, "# Your AGENTS.md\n\nlocal rule")

	if shared == -1 {
		t.Error("shared guidelines section missing or unlabeled")
	}
	if local == -1 {
		t.Error("agent AGENTS.md section missing or unlabeled")
	}
	if shared > local {
		t.Error("shared guidelines should precede the agent's AGENTS.md")
	}
}

func TestUnframe(t *testing.T) {
	msg := channel.Message{
		Channel:      "cli",
		Conversation: "main",
		Sender:       "user",
		Agent:        "main",
		Content:      "hello\nworld",
	}

	if got := Unframe(frameMessage(msg)); got != msg.Content {
		t.Errorf("Unframe(frameMessage) = %q, want %q", got, msg.Content)
	}
	if got := Unframe("plain text"); got != "plain text" {
		t.Errorf("Unframe(plain) = %q", got)
	}
}

func TestFrameMessage(t *testing.T) {
	out := frameMessage(channel.Message{
		Channel:      "cli",
		Conversation: "main",
		Sender:       "user",
		Agent:        "main",
		Content:      "hello",
	})

	if !strings.HasPrefix(out, `<message channel="cli" sender="user" time="`) {
		t.Errorf("unexpected envelope: %q", out)
	}
	if !strings.HasSuffix(out, ">\nhello\n</message>") {
		t.Errorf("content not framed: %q", out)
	}
	if strings.Contains(out, "conversation=") {
		t.Error("conversation matching the agent name should be omitted")
	}

	out = frameMessage(channel.Message{
		Channel:      "whatsapp",
		Conversation: "chat-42",
		Sender:       "adrian",
		Agent:        "main",
		Content:      "hi",
	})

	if !strings.Contains(out, `conversation="chat-42"`) {
		t.Errorf("distinct conversation should be included: %q", out)
	}
}

func TestTurnText(t *testing.T) {
	var buf strings.Builder

	tw := &turnText{sink: func(s string) { buf.WriteString(s) }}

	deltas := func(parts ...string) {
		for _, p := range parts {
			tw.add(agent.Message{Role: agent.RoleAssistant, Content: []agent.Content{{Text: p}}})
		}
	}

	tw.add(agent.Message{Role: agent.RoleAssistant, Content: []agent.Content{{Reasoning: &agent.Reasoning{ID: "r1", Summary: "thinking"}}}})
	deltas("Let me", " check.")
	tw.add(agent.Message{Role: agent.RoleAssistant, Content: []agent.Content{{ToolCall: &agent.ToolCall{ID: "1", Name: "shell"}}}})
	tw.add(agent.Message{Role: agent.RoleAssistant, Content: []agent.Content{{ToolResult: &agent.ToolResult{ID: "1", Content: "ok"}}}})
	deltas("All", " done.")

	if got, want := buf.String(), "Let me check.\n\nAll done."; got != want {
		t.Errorf("turnText = %q, want %q", got, want)
	}
}
