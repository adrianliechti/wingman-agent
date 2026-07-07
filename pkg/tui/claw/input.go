package claw

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (t *TUI) submitInput() {
	text := strings.TrimSpace(t.input.GetText())
	t.input.SetText("")

	if text == "" {
		return
	}

	if text == "/help" {
		t.writeHelp()
		return
	}

	name := t.selected()

	t.writeFormatted(text, false)
	t.chatView.ScrollToEnd()

	msg := channel.Message{
		Channel:      t.Name(),
		Conversation: name,
		Sender:       "user",
		Agent:        name,
		Content:      text,
	}

	t.setBusy(name, 1)
	t.refreshAgents()
	t.updateStatusBar()

	go func() {
		ctx := t.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		t.handler(ctx, msg)

		t.app.QueueUpdateDraw(func() {
			t.rerenderChat(name)
			t.setBusy(name, -1)

			if messages, _, ok := t.claw.AgentState(name); ok && name == t.selected() {
				t.renderedCount = len(messages)
			}

			t.refreshAgents()
			t.updateStatusBar()
		})
	}()
}

func (t *TUI) writeHelp() {
	th := theme.Default

	lines := []string{
		"enter   send message",
		"tab     cycle focus (input / agents)",
		"ctrl+c  quit",
		"/help   show this help",
	}

	for _, line := range lines {
		fmt.Fprintf(t.chatView, "%s[%s]\u2503[-] [%s]%s[-]\n", indent, th.BrBlack, th.BrBlack, line)
	}

	fmt.Fprintln(t.chatView)
	t.chatView.ScrollToEnd()
}
