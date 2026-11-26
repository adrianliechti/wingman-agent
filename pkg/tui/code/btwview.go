package code

import (
	"context"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

// btwOverlay shows one side question and its answer as it arrives. Neither
// enters the conversation unless the user adds the answer to the prompt.
type btwOverlay struct {
	app      *App
	question string
	cancel   context.CancelFunc

	mu       sync.Mutex
	streamed strings.Builder

	done   bool
	answer string
	err    error

	pager  pager
	height int
}

func (a *App) askAside(question string) {
	provider, ok := a.agent.(sideQuestioner)
	if !ok {
		a.appendChat(cellNotice("Side questions are unavailable for this agent", theme.Default.Yellow, a.width()))
		return
	}
	if question == "" {
		a.appendChat(cellNotice("Ask a side question with /btw <question>", theme.Default.BrBlack, a.width()))
		return
	}

	ctx, cancel := context.WithCancel(a.ctx)
	o := &btwOverlay{app: a, question: question, cancel: cancel}
	o.pager.title = "btw · side question"
	a.openOverlay(o)

	id := a.sessionID
	go func() {
		answer, err := provider.Ask(ctx, id, question, func(text string) {
			o.mu.Lock()
			o.streamed.WriteString(text)
			o.mu.Unlock()
			a.requestRender()
		})
		cancel()
		a.post(func() {
			o.done, o.answer, o.err = true, answer, err
			a.invalidate()
		})
	}()
}

func (o *btwOverlay) Close() {
	o.cancel()
}

func (o *btwOverlay) text() string {
	if o.done {
		return o.answer
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.streamed.String()
}

func (o *btwOverlay) Render(width, height int) []string {
	o.height = height

	// The pager keeps the last column for its scrollbar.
	inner := max(width-1, 1)
	lines := cellUser(o.question, inner)
	text := o.text()
	switch {
	case o.err != nil:
		lines = append(lines, cellError("Side question failed", o.err.Error(), inner)...)
	case text != "":
		lines = append(lines, cellAssistant(text, inner, theme.Default.Cyan, o.app.agent.Workspace().RootPath)...)
	case o.done:
		lines = append(lines, cellNotice("No answer", theme.Default.Yellow, inner)...)
	default:
		lines = append(lines, cellIndent+dim("Thinking…"))
	}

	o.pager.lines = lines
	switch {
	case !o.done:
		o.pager.hints = dim("esc cancel")
	case o.err == nil && o.answer != "":
		o.pager.hints = dim("↑↓/jk/wheel scroll · i add to prompt · esc back to chat")
	default:
		o.pager.hints = dim("esc back to chat")
	}

	return o.pager.Render(width, height)
}

func (o *btwOverlay) HandleKey(ev inline.KeyEvent) bool {
	if ev.Key == inline.KeyRune && ev.Rune == 'i' && o.done && o.err == nil && o.answer != "" {
		o.app.editor.Insert("Side question: " + o.question + "\nAnswer: " + o.answer + "\n")
		o.app.syncCommandPopup()
		return true
	}
	return o.pager.HandleKey(ev, o.height)
}

func (o *btwOverlay) HandleMouse(ev inline.MouseEvent) {
	o.pager.HandleMouse(ev, o.height)
}
