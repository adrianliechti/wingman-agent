package code

import (
	"context"
	"strings"
	"testing"

	corecode "github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func TestShellComposerChromeAndEscExit(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 80, 20)
	a.editor.SetText("!git status")

	chrome := a.composerChrome(80)
	if chrome.TopColor != theme.Default.Magenta {
		t.Fatalf("TopColor = %v, want Magenta (%v)", chrome.TopColor, theme.Default.Magenta)
	}
	if ansi.Strip(chrome.TopLabel) != "SHELL" {
		t.Fatalf("TopLabel = %q, want SHELL", ansi.Strip(chrome.TopLabel))
	}

	footer := ansi.Strip(a.footerLine(80))
	if !strings.Contains(footer, "enter run command") || !strings.Contains(footer, "esc exit shell") {
		t.Fatalf("shell footer = %q, want run/exit hints", footer)
	}

	// Bare ! exits shell mode when Esc is pressed.
	a.editor.SetText("!")
	a.handleKey(inline.KeyEvent{Key: inline.KeyEsc})
	if a.editor.Text() != "" {
		t.Fatalf("editor text after Esc on bare ! = %q, want empty", a.editor.Text())
	}
	if a.isShellInput() {
		t.Fatal("expected shell input mode to be false after Esc")
	}
}

func TestBareBangShowsNotice(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 80, 20)
	a.editor.SetText("!")
	a.submitInput()

	if a.editor.Text() != "" {
		t.Fatalf("editor text = %q, want empty", a.editor.Text())
	}
	joined := ansi.Strip(strings.Join(a.chat, "\n"))
	if !strings.Contains(joined, "Run a command with !<cmd>") {
		t.Fatalf("chat missing notice, got:\n%s", joined)
	}
	if len(a.agent.Messages(a.sessionID)) != 0 {
		t.Fatalf("bare ! should not record session messages, got %d", len(a.agent.Messages(a.sessionID)))
	}
}

func TestUserShellCommandExecutesPrintsOutputAndRecordsForAgent(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 80, 20)
	a.ctx = context.Background()
	a.queue = make(chan func(), 8)
	a.agent.(*uiTestAgent).workspace.RootPath = t.TempDir()

	a.editor.SetText("!printf 'hello-shell\\nline-two\\n'")
	a.submitInput()
	waitForSessionOperation(t, a)

	msgs := a.agent.Messages(a.sessionID)
	if len(msgs) != 1 {
		t.Fatalf("recorded messages = %d, want 1", len(msgs))
	}
	cmd, output, exitCode, ok := corecode.ParseUserShellCommand(msgs[0].Content[0].AsText())
	if !ok {
		t.Fatalf("recorded message is not <user_shell_command>:\n%s", msgs[0].Content[0].AsText())
	}
	if cmd != "printf 'hello-shell\\nline-two\\n'" || exitCode != 0 {
		t.Fatalf("parsed (%q, %d), want command with exit 0", cmd, exitCode)
	}
	if output != "hello-shell\nline-two" {
		t.Fatalf("parsed output = %q, want %q", output, "hello-shell\nline-two")
	}

	chatText := ansi.Strip(strings.Join(a.chat, "\n"))
	for _, want := range []string{"$ printf 'hello-shell\\nline-two\\n'", "hello-shell", "line-two"} {
		if !strings.Contains(chatText, want) {
			t.Fatalf("chat missing %q:\n%s", want, chatText)
		}
	}

	a.showTranscript()
	if a.overlay == nil {
		t.Fatal("showTranscript did not open transcript overlay")
	}
	rendered := ansi.Strip(strings.Join(a.overlay.Render(80, 20), "\n"))
	if !strings.Contains(rendered, "$ printf 'hello-shell\\nline-two\\n'") || !strings.Contains(rendered, "hello-shell") {
		t.Fatalf("transcript overlay missing shell output:\n%s", rendered)
	}
	if strings.Contains(rendered, "<user_shell_command>") {
		t.Fatalf("raw <user_shell_command> leaked into transcript:\n%s", rendered)
	}
}
