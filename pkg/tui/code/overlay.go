package code

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

// Overlay is a full-screen view replacing the chat frame. HandleKey returns
// true when the overlay should close.
type Overlay interface {
	HandleKey(ev inline.KeyEvent) bool
	Render(width, height int) []string
}

func (a *App) openOverlay(o Overlay) {
	a.overlay = o
	a.invalidate()
}

func (a *App) closeOverlay() {
	a.overlay = nil
	a.invalidate()
}

// pager is a scrollable line view with a title header and hint footer.
type pager struct {
	title string
	lines []string
	hints string

	offset int
}

func (p *pager) contentRows(height int) int {
	return height - 3
}

func (p *pager) clamp(height int) {
	maxOffset := len(p.lines) - p.contentRows(height)
	if p.offset > maxOffset {
		p.offset = maxOffset
	}
	if p.offset < 0 {
		p.offset = 0
	}
}

func (p *pager) HandleKey(ev inline.KeyEvent, height int) bool {
	switch ev.Key {
	case inline.KeyEsc:
		return true
	case inline.KeyUp:
		p.offset--
	case inline.KeyDown:
		p.offset++
	case inline.KeyPgUp:
		p.offset -= p.contentRows(height)
	case inline.KeyPgDn:
		p.offset += p.contentRows(height)
	case inline.KeyHome:
		p.offset = 0
	case inline.KeyEnd:
		p.offset = len(p.lines)
	case inline.KeyCtrl:
		if ev.Rune == 'c' {
			return true
		}
	case inline.KeyRune:
		switch ev.Rune {
		case 'q':
			return true
		case 'j':
			p.offset++
		case 'k':
			p.offset--
		case 'g':
			p.offset = 0
		case 'G':
			p.offset = len(p.lines)
		}
	}

	p.clamp(height)
	return false
}

func (p *pager) HandleMouse(ev inline.MouseEvent, height int) {
	if ev.Kind == inline.MouseWheel {
		p.offset += ev.WheelDelta * 3
		p.clamp(height)
	}
}

func (p *pager) Render(width, height int) []string {
	t := theme.Default
	p.clamp(height)

	percent := 100
	rows := p.contentRows(height)
	if len(p.lines) > rows {
		percent = (p.offset + rows) * 100 / len(p.lines)
	}

	head := cellIndent + bold(p.title)
	ruleWidth := width - 2*len(cellIndent)
	if ruleWidth < 10 {
		ruleWidth = 10
	}
	pct := fmt.Sprintf(" %d%% ", percent)
	rule := strings.Repeat("─", ruleWidth-ansi.Width(pct)-2) + pct + "──"

	lines := []string{head, cellIndent + colored(t.BrBlack, rule)}

	end := p.offset + rows
	if end > len(p.lines) {
		end = len(p.lines)
	}

	lines = append(lines, p.lines[p.offset:end]...)

	for len(lines) < height-1 {
		lines = append(lines, "")
	}

	lines = append(lines, cellIndent+p.hints)

	return lines
}

// transcriptOverlay shows the full session with nothing truncated,
// Claude-style: opened and closed with ctrl+o.
type transcriptOverlay struct {
	pager  pager
	height int
}

func (a *App) showTranscript() {
	width := a.width()

	var lines []string

	for _, msg := range a.agent.Messages(a.sessionID) {
		if msg.Hidden {
			continue
		}
		for _, c := range msg.Content {
			switch {
			case c.ToolResult != nil:
				if a.isToolHidden(c.ToolResult.Name) {
					continue
				}
				lines = append(lines, cellTool(c.ToolResult, width, true)...)
			case c.Reasoning != nil && c.Reasoning.Summary != "":
				lines = append(lines, cellReasoning(c.Reasoning.Summary, width, true)...)
				lines = append(lines, "")
			case c.Text != "":
				switch msg.Role {
				case agent.RoleUser:
					lines = append(lines, cellUser(c.Text, width)...)
				case agent.RoleAssistant:
					lines = append(lines, cellAssistant(c.Text, width, theme.Default.Green)...)
				}
			}
		}
	}

	_, _, streamingText, streamingReasoning := a.snapshotStreamState()
	if streamingReasoning != "" {
		lines = append(lines, cellReasoning(streamingReasoning, width, true)...)
	}
	if streamingText != "" {
		lines = append(lines, cellAssistant(streamingText, width, theme.Default.BrBlack)...)
	}

	o := &transcriptOverlay{
		pager: pager{
			title: "transcript",
			lines: lines,
			hints: dim("↑↓/jk/wheel scroll · g/G top/bottom · ctrl+o close"),
		},
	}
	o.pager.offset = len(lines)

	a.openOverlay(o)
}

func (o *transcriptOverlay) HandleKey(ev inline.KeyEvent) bool {
	if ev.Key == inline.KeyCtrl && ev.Rune == 'o' {
		return true
	}
	return o.pager.HandleKey(ev, o.height)
}

func (o *transcriptOverlay) HandleMouse(ev inline.MouseEvent) {
	o.pager.HandleMouse(ev, o.height)
}

func (o *transcriptOverlay) Render(width, height int) []string {
	o.height = height
	return o.pager.Render(width, height)
}
