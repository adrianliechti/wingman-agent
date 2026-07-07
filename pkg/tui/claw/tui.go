package claw

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/rivo/tview"

	"github.com/adrianliechti/wingman-agent/pkg/claw"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
	"github.com/adrianliechti/wingman-agent/pkg/tui"
	"github.com/adrianliechti/wingman-agent/pkg/tui/markdown"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type TUI struct {
	claw    *claw.Claw
	app     *tview.Application
	ctx     context.Context
	handler channel.MessageHandler

	agentList *tview.List
	taskView  *tview.TextView
	chatView  *tview.TextView
	input     *tview.InputField
	statusBar *tview.TextView

	selectedAgent string
	agentNames    []string
	busy          map[string]int
	renderedCount int
	chatWidth     int
	mu            sync.Mutex
}

func New(c *claw.Claw) *TUI {
	return &TUI{
		claw:          c,
		selectedAgent: "main",
		busy:          map[string]int{},
	}
}

func (t *TUI) Name() string { return "cli" }

func (t *TUI) Start(ctx context.Context, handler channel.MessageHandler) error {
	t.ctx = ctx
	t.handler = handler

	theme.Auto()
	t.buildUI()
	t.refreshAgents()
	t.selectAgent("main")

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.app.QueueUpdateDraw(func() {
					t.refreshTasks()
					t.syncChat()
				})
			}
		}
	}()

	return t.app.Run()
}

func (t *TUI) Send(ctx context.Context, conversation string, text string) error {
	t.app.QueueUpdateDraw(func() {
		if conversation == t.selected() {
			t.writeFormatted(text, true)
			t.chatView.ScrollToEnd()
		}
	})

	return nil
}

func (t *TUI) SendStream(ctx context.Context, conversation string) (io.WriteCloser, error) {
	return &streamWriter{
		tui:  t,
		name: conversation,
	}, nil
}

func (t *TUI) cycleFocus() {
	switch t.app.GetFocus() {
	case t.input:
		t.app.SetFocus(t.agentList)
	case t.agentList:
		t.app.SetFocus(t.input)
	default:
		t.app.SetFocus(t.input)
	}
}

func (t *TUI) rerenderChat(name string) {
	if name != t.selected() {
		return
	}

	t.chatView.Clear()
	t.renderedCount = 0

	if messages, _, ok := t.claw.AgentState(name); ok {
		t.renderMessages(messages)
		t.renderedCount = len(messages)
	}
}

func (t *TUI) setBusy(name string, delta int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.busy[name] += delta

	if t.busy[name] <= 0 {
		delete(t.busy, name)
	}
}

func (t *TUI) isBusy(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.busy[name] > 0
}

func (t *TUI) syncChat() {
	name := t.selected()

	if t.isBusy(name) {
		return
	}

	messages, _, ok := t.claw.AgentState(name)

	if !ok || len(messages) == t.renderedCount {
		return
	}

	t.renderedCount = len(messages)
	t.chatView.Clear()
	t.renderMessages(messages)
	t.updateStatusBar()
}

func (t *TUI) updateStatusBar() {
	th := theme.Default
	name := t.selected()
	_, usage, ok := t.claw.AgentState(name)

	t.statusBar.Clear()

	if t.isBusy(name) {
		fmt.Fprintf(t.statusBar, "  [%s]working\u2026[-] [%s]\u2503[-]", th.Yellow, th.BrBlack)
	}

	if !ok {
		fmt.Fprintf(t.statusBar, "  [%s]%s[-] ", th.Cyan, name)
		return
	}

	if usage.CachedTokens > 0 {
		fmt.Fprintf(t.statusBar, "  [%s]\u2191%s (%s cached) \u2193%s[-] [%s]\u2503[-] [%s]%s[-] ",
			th.BrBlack,
			tui.FormatTokens(usage.InputTokens),
			tui.FormatTokens(usage.CachedTokens),
			tui.FormatTokens(usage.OutputTokens),
			th.BrBlack,
			th.Cyan,
			name,
		)
		return
	}

	fmt.Fprintf(t.statusBar, "  [%s]\u2191%s \u2193%s[-] [%s]\u2503[-] [%s]%s[-] ",
		th.BrBlack,
		tui.FormatTokens(usage.InputTokens),
		tui.FormatTokens(usage.OutputTokens),
		th.BrBlack,
		th.Cyan,
		name,
	)
}

type streamWriter struct {
	tui  *TUI
	name string
	buf  string
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.buf += string(p)

	var lines []string
	for {
		line, rest, ok := strings.Cut(w.buf, "\n")
		if !ok {
			break
		}
		lines = append(lines, line)
		w.buf = rest
	}

	if len(lines) > 0 {
		w.writeLines(lines)
	}

	return len(p), nil
}

func (w *streamWriter) writeLines(lines []string) {
	th := theme.Default

	w.tui.app.QueueUpdateDraw(func() {
		if w.name != w.tui.selected() {
			return
		}

		for _, line := range lines {
			for _, wl := range markdown.WrapLine(tview.Escape(line), w.tui.contentWidth()) {
				fmt.Fprintf(w.tui.chatView, "%s[%s]\u2503[-] %s\n", indent, th.Blue, wl)
			}
		}

		w.tui.chatView.ScrollToEnd()
	})
}

func (w *streamWriter) Close() error {
	if w.buf != "" {
		w.writeLines([]string{w.buf})
		w.buf = ""
	}

	w.tui.app.QueueUpdateDraw(func() {
		if w.name == w.tui.selected() {
			fmt.Fprintln(w.tui.chatView)
		}
	})

	return nil
}
