package code

import (
	"fmt"
	"strings"

	"github.com/sahilm/fuzzy"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

const popupMaxRows = 8

type PopupItem struct {
	ID     string
	Label  string
	Detail string
}

type popupKind int

const (
	popupCommands popupKind = iota
	popupFiles
	popupList
)

// Popup is the single list component behind slash-command completion, file
// mentions, and the model/effort/rewind pickers. It renders below the
// composer. Standalone pickers (popupList) capture all input; the other kinds
// filter as the user types in the editor.
type Popup struct {
	kind   popupKind
	title  string
	header []string

	items    []PopupItem
	filtered []int
	index    int
	offset   int

	multi    bool
	selected map[string]bool

	query    string
	accepted bool

	hotkeys  map[rune]string
	onAccept func(ids []string)
	onCancel func()
}

func newPopup(kind popupKind, title string, items []PopupItem, onAccept func(ids []string)) *Popup {
	p := &Popup{
		kind:     kind,
		title:    title,
		items:    items,
		selected: map[string]bool{},
		onAccept: onAccept,
	}
	p.SetQuery("")
	return p
}

type popupSource []PopupItem

func (s popupSource) String(i int) string { return s[i].Label }
func (s popupSource) Len() int            { return len(s) }

func (p *Popup) SetQuery(query string) {
	p.query = query
	p.filtered = p.filtered[:0]

	switch {
	case query == "":
		for i := range p.items {
			p.filtered = append(p.filtered, i)
		}
	case p.kind == popupCommands:
		for i, item := range p.items {
			if strings.HasPrefix(item.Label, query) {
				p.filtered = append(p.filtered, i)
			}
		}
	default:
		for _, m := range fuzzy.FindFrom(query, popupSource(p.items)) {
			p.filtered = append(p.filtered, m.Index)
		}
	}

	if p.index >= len(p.filtered) {
		p.index = 0
	}
	p.offset = 0
}

func (p *Popup) SetSelected(id string, on bool) {
	if on {
		p.selected[id] = true
	} else {
		delete(p.selected, id)
	}
}

func (p *Popup) Select(index int) {
	if index >= 0 && index < len(p.filtered) {
		p.index = index
	}
}

func (p *Popup) SelectID(id string) {
	for i, idx := range p.filtered {
		if p.items[idx].ID == id {
			p.index = i
			return
		}
	}
}

func (p *Popup) Empty() bool {
	return len(p.filtered) == 0
}

func (p *Popup) Current() (PopupItem, bool) {
	if p.index < 0 || p.index >= len(p.filtered) {
		return PopupItem{}, false
	}
	return p.items[p.filtered[p.index]], true
}

func (p *Popup) accept() {
	var ids []string

	if p.multi && len(p.selected) > 0 {
		for _, idx := range p.filtered {
			if p.selected[p.items[idx].ID] {
				ids = append(ids, p.items[idx].ID)
			}
		}
		for id := range p.selected {
			found := false
			for _, have := range ids {
				if have == id {
					found = true
					break
				}
			}
			if !found {
				ids = append(ids, id)
			}
		}
	} else if item, ok := p.Current(); ok {
		ids = []string{item.ID}
	}

	if len(ids) > 0 {
		p.accepted = true
		if p.onAccept != nil {
			p.onAccept(ids)
		}
	}
}

func (p *Popup) acceptID(id string) {
	p.accepted = true
	if p.onAccept != nil {
		p.onAccept([]string{id})
	}
}

// HandleKey processes navigation. Returns (consumed, closed).
func (p *Popup) HandleKey(ev inline.KeyEvent) (bool, bool) {
	switch ev.Key {
	case inline.KeyUp:
		if p.index > 0 {
			p.index--
		}
		return true, false

	case inline.KeyDown:
		if p.index < len(p.filtered)-1 {
			p.index++
		}
		return true, false

	case inline.KeyPgUp:
		p.index -= popupMaxRows
		if p.index < 0 {
			p.index = 0
		}
		return true, false

	case inline.KeyPgDn:
		p.index += popupMaxRows
		if p.index >= len(p.filtered) {
			p.index = len(p.filtered) - 1
		}
		return true, false

	case inline.KeyTab:
		if p.multi {
			if item, ok := p.Current(); ok {
				p.SetSelected(item.ID, !p.selected[item.ID])
				if p.index < len(p.filtered)-1 {
					p.index++
				}
			}
			return true, false
		}
		if p.kind == popupCommands {
			p.accept()
			return true, true
		}
		return false, false

	case inline.KeyEnter:
		p.accept()
		return true, true

	case inline.KeyEsc:
		return true, true
	}

	if p.kind == popupList {
		switch ev.Key {
		case inline.KeyRune, inline.KeyBackspace:
			if ev.Key == inline.KeyBackspace {
				if p.query != "" {
					r := []rune(p.query)
					p.SetQuery(string(r[:len(r)-1]))
				}
			} else if !ev.Alt {
				if id, ok := p.hotkeys[ev.Rune]; ok {
					p.acceptID(id)
					return true, true
				}
				if p.multi && ev.Rune == ' ' {
					if item, ok := p.Current(); ok {
						p.SetSelected(item.ID, !p.selected[item.ID])
					}
					return true, false
				}
				p.SetQuery(p.query + string(ev.Rune))
			}
			return true, false
		case inline.KeyCtrl:
			if ev.Rune == 'c' {
				return true, true
			}
		}
		return true, false
	}

	return false, false
}

func (p *Popup) Render(width int) []string {
	t := theme.Default

	var lines []string

	lines = append(lines, p.header...)

	if p.title != "" {
		title := p.title
		if p.kind == popupList && p.query != "" {
			title += "  " + p.query
		}
		lines = append(lines, cellIndent+dim(title))
	}

	if len(p.filtered) == 0 {
		lines = append(lines, cellIndent+dim("  no matches"))
		return lines
	}

	visible := popupMaxRows
	if p.index < p.offset {
		p.offset = p.index
	}
	if p.index >= p.offset+visible {
		p.offset = p.index - visible + 1
	}

	end := p.offset + visible
	if end > len(p.filtered) {
		end = len(p.filtered)
	}

	inner := width - len(cellIndent)
	if inner < 20 {
		inner = 20
	}

	for i := p.offset; i < end; i++ {
		item := p.items[p.filtered[i]]

		marker := "  "
		if p.multi {
			marker = dim("□ ")
			if p.selected[item.ID] {
				marker = colored(t.Cyan, "■ ")
			}
		}

		var line string
		if i == p.index {
			line = colored(t.Cyan, "→ ") + marker + fg(t.Cyan) + item.Label + ansi.Reset
		} else {
			line = "  " + marker + item.Label
		}

		if item.Detail != "" {
			line += "  " + dim(item.Detail)
		}

		lines = append(lines, cellIndent+ansi.Truncate(line, inner, "…")+ansi.Reset)
	}

	if len(p.filtered) > visible {
		lines = append(lines, cellIndent+dim(fmt.Sprintf("  (%d/%d)", p.index+1, len(p.filtered))))
	}

	return lines
}
