package claw

import (
	"context"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
)

func (t *TUI) submitInput() {
	text := strings.TrimSpace(string(t.input))
	t.input = nil
	t.inputCursor = 0

	if text == "" {
		return
	}

	if text == "/help" {
		t.writeHelp()
		return
	}

	name := t.selected()

	t.appendChat(formatChatMessage(text, false, t.chatWidth()))

	msg := channel.Message{
		Channel:      t.Name(),
		Conversation: name,
		Sender:       "user",
		Agent:        name,
		Content:      text,
	}

	t.setBusy(name, 1)
	t.refreshAgents()

	go func() {
		ctx := t.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		t.handler(ctx, msg)

		t.post(func() {
			t.setBusy(name, -1)
			t.rerenderChat(name)
			t.refreshAgents()
		})
	}()
}

func (t *TUI) writeHelp() {
	lines := []string{
		"enter   send message",
		"tab     cycle focus (input / agents)",
		"pgup/pgdn  scroll chat",
		"ctrl+c  quit",
		"/help   show this help",
	}

	for _, line := range lines {
		t.appendChat([]string{indent + dim(line)})
	}
	t.appendChat([]string{""})
}
