package claw

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/claw"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type focusArea int

const (
	focusInput focusArea = iota
	focusAgents
)

type TUI struct {
	claw    *claw.Claw
	ctx     context.Context
	handler channel.MessageHandler

	term  *inline.Terminal
	queue chan func()
	quit  chan struct{}
	once  sync.Once

	focus focusArea

	input       []rune
	inputCursor int

	selectedAgent string
	agentNames    []string
	agentIndex    int

	chatLines  []string
	chatScroll int
	follow     bool

	taskLines []string

	busy          map[string]int
	renderedCount int

	mu sync.Mutex
}

func New(c *claw.Claw) *TUI {
	return &TUI{
		claw:          c,
		selectedAgent: "main",
		busy:          map[string]int{},
		follow:        true,
		queue:         make(chan func(), 64),
		quit:          make(chan struct{}),
	}
}

func (t *TUI) Name() string { return "cli" }

func (t *TUI) post(fn func()) {
	select {
	case t.queue <- fn:
	case <-t.quit:
	}
}

func (t *TUI) stop() {
	t.once.Do(func() {
		close(t.quit)
	})
}

func (t *TUI) Start(ctx context.Context, handler channel.MessageHandler) error {
	t.ctx = ctx
	t.handler = handler

	theme.Auto()

	t.term = inline.NewTerminal()
	if err := t.term.Start(); err != nil {
		return err
	}

	// Closing quit turns t.post into a no-op so a still-streaming agent can
	// never wedge against a stopped UI (and with it, session teardown).
	defer t.stop()

	t.term.EnterAlt()

	t.refreshAgents()
	t.selectAgent("main")

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	t.render()

	for {
		select {
		case <-ctx.Done():
			t.term.Stop()
			return nil

		case <-t.quit:
			t.term.Stop()
			return nil

		case <-ticker.C:
			t.refreshTasks()
			t.syncChat()

		case fn := <-t.queue:
			fn()
			for {
				select {
				case fn := <-t.queue:
					fn()
					continue
				default:
				}
				break
			}

		case ev := <-t.term.Events():
			switch ev := ev.(type) {
			case inline.ResizeEvent:
				t.term.Resized(ev.Width, ev.Height)
				t.rerenderChat(t.selected())
			case inline.PasteEvent:
				t.insertInput(strings.ReplaceAll(ev.Text, "\n", " "))
			case inline.KeyEvent:
				if quit := t.handleKey(ev); quit {
					t.term.Stop()
					return nil
				}
			}
		}

		t.render()
	}
}

func (t *TUI) Send(ctx context.Context, conversation string, text string) error {
	t.post(func() {
		if conversation == t.selected() {
			t.appendChat(formatChatMessage(text, true, t.chatWidth()))
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

func (t *TUI) selected() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.selectedAgent
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

func (t *TUI) appendChat(lines []string) {
	t.chatLines = append(t.chatLines, lines...)
	if t.follow {
		t.chatScroll = len(t.chatLines)
	}
}

func (t *TUI) rerenderChat(name string) {
	if name != t.selected() {
		return
	}

	t.chatLines = nil
	t.renderedCount = 0

	if messages, _, ok := t.claw.AgentState(name); ok {
		t.appendChat(formatMessages(messages, t.chatWidth()))
		t.renderedCount = len(messages)
	}
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
	t.chatLines = nil
	t.appendChat(formatMessages(messages, t.chatWidth()))
}

func (t *TUI) selectAgent(name string) {
	t.mu.Lock()
	t.selectedAgent = name
	t.mu.Unlock()

	t.follow = true
	t.rerenderChat(name)
	t.refreshTasks()
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
	w.tui.post(func() {
		if w.name != w.tui.selected() {
			return
		}

		for _, line := range lines {
			w.tui.appendChat(formatStreamLine(line, w.tui.chatWidth()))
		}
	})
}

func (w *streamWriter) Close() error {
	if w.buf != "" {
		w.writeLines([]string{w.buf})
		w.buf = ""
	}

	w.tui.post(func() {
		if w.name == w.tui.selected() {
			w.tui.appendChat([]string{""})
		}
	})

	return nil
}
