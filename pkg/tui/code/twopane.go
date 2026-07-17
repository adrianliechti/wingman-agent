package code

import (
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

const paneListWidth = 40

// twoPaneOverlay is the shared frame behind /diff and /problems: a selectable
// file list left, detail content right.
type twoPaneOverlay struct {
	title  string
	status string

	items   func(selected bool, index int) string
	count   int
	content func(index int) []string

	selected     int
	focusRight   bool
	listOffset   int
	detailOffset int
	detail       []string
	height       int
}

func newTwoPaneOverlay(title, status string, count int, item func(selected bool, index int) string, content func(index int) []string) *twoPaneOverlay {
	o := &twoPaneOverlay{
		title:   title,
		status:  status,
		items:   item,
		count:   count,
		content: content,
	}
	o.loadDetail()
	return o
}

func (o *twoPaneOverlay) loadDetail() {
	o.detail = nil
	o.detailOffset = 0
	if o.content != nil && o.selected >= 0 && o.selected < o.count {
		o.detail = o.content(o.selected)
	}
}

func (o *twoPaneOverlay) moveSelection(delta int) {
	next := o.selected + delta
	if next < 0 || next >= o.count {
		return
	}
	o.selected = next
	o.loadDetail()
}

func (o *twoPaneOverlay) HandleKey(ev inline.KeyEvent) bool {
	rows := o.height - 3

	scrollDetail := func(delta int) {
		o.detailOffset += delta
		if o.detailOffset > len(o.detail)-rows {
			o.detailOffset = len(o.detail) - rows
		}
		if o.detailOffset < 0 {
			o.detailOffset = 0
		}
	}

	switch ev.Key {
	case inline.KeyEsc:
		return true
	case inline.KeyTab:
		o.focusRight = !o.focusRight
		return false
	case inline.KeyUp:
		if o.focusRight {
			scrollDetail(-1)
		} else {
			o.moveSelection(-1)
		}
		return false
	case inline.KeyDown:
		if o.focusRight {
			scrollDetail(1)
		} else {
			o.moveSelection(1)
		}
		return false
	case inline.KeyPgUp:
		scrollDetail(-rows)
		return false
	case inline.KeyPgDn:
		scrollDetail(rows)
		return false
	case inline.KeyCtrl:
		return ev.Rune == 'c'
	case inline.KeyRune:
		switch ev.Rune {
		case 'q':
			return true
		case 'j':
			if o.focusRight {
				scrollDetail(1)
			} else {
				o.moveSelection(1)
			}
		case 'k':
			if o.focusRight {
				scrollDetail(-1)
			} else {
				o.moveSelection(-1)
			}
		case 'g':
			scrollDetail(-len(o.detail))
		case 'G':
			scrollDetail(len(o.detail))
		}
	}

	return false
}

func (o *twoPaneOverlay) Render(width, height int) []string {
	t := theme.Default
	o.height = height
	rows := height - 3

	listWidth := paneListWidth
	if width < 80 {
		listWidth = width / 3
	}
	detailWidth := width - listWidth - 3

	if o.selected < o.listOffset {
		o.listOffset = o.selected
	}
	if o.selected >= o.listOffset+rows {
		o.listOffset = o.selected - rows + 1
	}

	head := cellIndent + bold(o.title) + "  " + o.status
	rule := cellIndent + colored(t.BrBlack, strings.Repeat("─", max(10, width-2*len(cellIndent))))

	lines := []string{head, rule}

	for i := 0; i < rows; i++ {
		var left, right string

		if idx := o.listOffset + i; idx < o.count {
			left = o.items(idx == o.selected && !o.focusRight, idx)
		}

		if idx := o.detailOffset + i; idx < len(o.detail) {
			right = o.detail[idx]
		}

		row := cellIndent + ansi.Pad(left, listWidth) + colored(t.BrBlack, "│") + " " + ansi.Truncate(right, detailWidth, "…")
		lines = append(lines, row)
	}

	hints := dim("↑↓/jk select · tab switch pane · esc close")
	if o.focusRight {
		hints = dim("↑↓/jk scroll · g/G top/bottom · tab switch pane · esc close")
	}
	lines = append(lines, cellIndent+hints)

	return lines
}
