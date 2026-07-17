package code

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

// Editor is a multiline input bounded by horizontal rules. The rule color is
// a status channel (mode, activity) set by the app.
type Editor struct {
	value  []rune
	cursor int

	placeholder string
	ruleColor   ansi.Color

	history []string
	histIdx int
	draft   string

	scroll int
}

func NewEditor() *Editor {
	return &Editor{
		placeholder: "Ask anything...",
		ruleColor:   theme.Default.BrBlack,
		histIdx:     -1,
	}
}

func (e *Editor) Text() string {
	return string(e.value)
}

func (e *Editor) SetText(text string) {
	e.value = []rune(text)
	e.cursor = len(e.value)
}

func (e *Editor) SetPlaceholder(p string) {
	e.placeholder = p
}

func (e *Editor) SetRuleColor(c ansi.Color) {
	e.ruleColor = c
}

func (e *Editor) AddHistory(entry string) {
	if entry == "" {
		return
	}
	if n := len(e.history); n > 0 && e.history[n-1] == entry {
		e.histIdx = -1
		return
	}
	e.history = append(e.history, entry)
	e.histIdx = -1
}

func (e *Editor) Insert(text string) {
	runes := []rune(text)
	e.value = append(e.value[:e.cursor], append(runes, e.value[e.cursor:]...)...)
	e.cursor += len(runes)
}

func (e *Editor) lineBounds() (start, end int) {
	start = e.cursor
	for start > 0 && e.value[start-1] != '\n' {
		start--
	}
	end = e.cursor
	for end < len(e.value) && e.value[end] != '\n' {
		end++
	}
	return
}

func (e *Editor) deleteRange(from, to int) {
	if from < 0 {
		from = 0
	}
	if to > len(e.value) {
		to = len(e.value)
	}
	if from >= to {
		return
	}
	e.value = append(e.value[:from], e.value[to:]...)
	if e.cursor > to {
		e.cursor -= to - from
	} else if e.cursor > from {
		e.cursor = from
	}
}

func isWordRune(r rune) bool {
	return r != ' ' && r != '\t' && r != '\n' && r != '/' && r != '.'
}

func (e *Editor) prevWord() int {
	i := e.cursor
	for i > 0 && !isWordRune(e.value[i-1]) {
		i--
	}
	for i > 0 && isWordRune(e.value[i-1]) {
		i--
	}
	return i
}

func (e *Editor) nextWord() int {
	i := e.cursor
	for i < len(e.value) && !isWordRune(e.value[i]) {
		i++
	}
	for i < len(e.value) && isWordRune(e.value[i]) {
		i++
	}
	return i
}

// HandleKey processes an input event. It reports whether the event was
// consumed; unconsumed navigation (history recall) is handled by the caller.
func (e *Editor) HandleKey(ev inline.KeyEvent) bool {
	switch ev.Key {
	case inline.KeyRune:
		if ev.Alt {
			switch ev.Rune {
			case 'b':
				e.cursor = e.prevWord()
				return true
			case 'f':
				e.cursor = e.nextWord()
				return true
			}
			return false
		}
		e.Insert(string(ev.Rune))
		return true

	case inline.KeyBackspace:
		if ev.Alt {
			e.deleteRange(e.prevWord(), e.cursor)
			return true
		}
		if e.cursor > 0 {
			e.deleteRange(e.cursor-1, e.cursor)
		}
		return true

	case inline.KeyDelete:
		if e.cursor < len(e.value) {
			e.deleteRange(e.cursor, e.cursor+1)
		}
		return true

	case inline.KeyLeft:
		if ev.Alt {
			e.cursor = e.prevWord()
			return true
		}
		if e.cursor > 0 {
			e.cursor--
		}
		return true

	case inline.KeyRight:
		if ev.Alt {
			e.cursor = e.nextWord()
			return true
		}
		if e.cursor < len(e.value) {
			e.cursor++
		}
		return true

	case inline.KeyUp:
		return e.moveVertical(-1)

	case inline.KeyDown:
		return e.moveVertical(1)

	case inline.KeyHome:
		start, _ := e.lineBounds()
		e.cursor = start
		return true

	case inline.KeyEnd:
		_, end := e.lineBounds()
		e.cursor = end
		return true

	case inline.KeyCtrl:
		switch ev.Rune {
		case 'a':
			start, _ := e.lineBounds()
			e.cursor = start
			return true
		case 'e':
			_, end := e.lineBounds()
			e.cursor = end
			return true
		case 'b':
			if e.cursor > 0 {
				e.cursor--
			}
			return true
		case 'f':
			if e.cursor < len(e.value) {
				e.cursor++
			}
			return true
		case 'u':
			start, _ := e.lineBounds()
			e.deleteRange(start, e.cursor)
			return true
		case 'k':
			_, end := e.lineBounds()
			e.deleteRange(e.cursor, end)
			return true
		case 'w':
			e.deleteRange(e.prevWord(), e.cursor)
			return true
		case 'j':
			e.Insert("\n")
			return true
		case 'd':
			if e.cursor < len(e.value) {
				e.deleteRange(e.cursor, e.cursor+1)
			}
			return true
		}
		return false
	}

	return false
}

// moveVertical moves the cursor across logical lines; returns false at the
// buffer edge so the app can use up/down for history.
func (e *Editor) moveVertical(delta int) bool {
	start, _ := e.lineBounds()
	col := e.cursor - start

	if delta < 0 {
		if start == 0 {
			return false
		}
		prevStart := start - 1
		for prevStart > 0 && e.value[prevStart-1] != '\n' {
			prevStart--
		}
		prevLen := start - 1 - prevStart
		if col > prevLen {
			col = prevLen
		}
		e.cursor = prevStart + col
		return true
	}

	_, end := e.lineBounds()
	if end >= len(e.value) {
		return false
	}
	nextStart := end + 1
	nextEnd := nextStart
	for nextEnd < len(e.value) && e.value[nextEnd] != '\n' {
		nextEnd++
	}
	if col > nextEnd-nextStart {
		col = nextEnd - nextStart
	}
	e.cursor = nextStart + col
	return true
}

func (e *Editor) HistoryPrev() bool {
	if len(e.history) == 0 {
		return false
	}
	if e.histIdx == -1 {
		e.draft = e.Text()
		e.histIdx = len(e.history)
	}
	if e.histIdx == 0 {
		return true
	}
	e.histIdx--
	e.SetText(e.history[e.histIdx])
	return true
}

func (e *Editor) HistoryNext() bool {
	if e.histIdx == -1 {
		return false
	}
	e.histIdx++
	if e.histIdx >= len(e.history) {
		e.histIdx = -1
		e.SetText(e.draft)
		return true
	}
	e.SetText(e.history[e.histIdx])
	return true
}

func (e *Editor) ResetHistoryCursor() {
	e.histIdx = -1
	e.draft = ""
}

type editorRow struct {
	text      string
	start     int
	runeCount int
}

// rows soft-wraps the buffer for display; start indexes let cursor position
// map to a row/col.
func (e *Editor) rows(inner int) []editorRow {
	if inner < 1 {
		inner = 1
	}

	var rows []editorRow
	lineStart := 0
	text := e.value

	flushLine := func(start, end int) {
		if start == end {
			rows = append(rows, editorRow{text: "", start: start})
			return
		}
		segStart := start
		width := 0
		lastSpace := -1
		for i := start; i < end; i++ {
			w := runeWidth(text[i])
			if text[i] == ' ' {
				lastSpace = i
			}
			if width+w > inner && i > segStart {
				breakAt := i
				if lastSpace > segStart {
					breakAt = lastSpace + 1
				}
				rows = append(rows, editorRow{text: string(text[segStart:breakAt]), start: segStart, runeCount: breakAt - segStart})
				segStart = breakAt
				lastSpace = -1
				width = 0
				for j := segStart; j <= i; j++ {
					width += runeWidth(text[j])
				}
				continue
			}
			width += w
		}
		rows = append(rows, editorRow{text: string(text[segStart:end]), start: segStart, runeCount: end - segStart})
	}

	for i := 0; i <= len(text); i++ {
		if i == len(text) || text[i] == '\n' {
			flushLine(lineStart, i)
			lineStart = i + 1
		}
	}

	return rows
}

func runeWidth(r rune) int {
	return ansi.Width(string(r))
}

// Render returns the editor lines (rules included) and the cursor position
// relative to the returned block.
func (e *Editor) Render(width, maxRows int) ([]string, inline.Pos) {
	t := theme.Default
	inner := width - 2*len(cellIndent) - 2

	rule := func(label string) string {
		line := strings.Repeat("─", width-2*len(cellIndent))
		if label != "" && len(label)+6 < width {
			line = "─── " + label + " " + strings.Repeat("─", width-2*len(cellIndent)-6-ansi.Width(label))
		}
		return cellIndent + colored(e.ruleColor, line)
	}

	rows := e.rows(inner)

	cursorRow, cursorCol := 0, 0
	for i, row := range rows {
		if e.cursor >= row.start && e.cursor <= row.start+row.runeCount {
			cursorRow = i
			cursorCol = ansi.Width(string([]rune(row.text)[:e.cursor-row.start]))
		}
	}

	if maxRows < 3 {
		maxRows = 3
	}
	visible := maxRows - 2

	if cursorRow < e.scroll {
		e.scroll = cursorRow
	}
	if cursorRow >= e.scroll+visible {
		e.scroll = cursorRow - visible + 1
	}
	if e.scroll > len(rows)-visible {
		e.scroll = len(rows) - visible
	}
	if e.scroll < 0 {
		e.scroll = 0
	}

	topLabel := ""
	if e.scroll > 0 {
		topLabel = dimLabel(e.scroll, "↑")
	}
	bottomLabel := ""
	if hidden := len(rows) - e.scroll - visible; hidden > 0 {
		bottomLabel = dimLabel(hidden, "↓")
	}

	lines := []string{rule(topLabel)}

	end := e.scroll + visible
	if end > len(rows) {
		end = len(rows)
	}

	if len(e.value) == 0 {
		lines = append(lines, cellIndent+" "+fg(t.BrBlack)+e.placeholder+ansi.Reset)
	} else {
		for _, row := range rows[e.scroll:end] {
			lines = append(lines, cellIndent+" "+row.text)
		}
	}

	lines = append(lines, rule(bottomLabel))

	cursor := inline.Pos{
		Row: 1 + cursorRow - e.scroll,
		Col: len(cellIndent) + 1 + cursorCol,
	}

	return lines, cursor
}

func dimLabel(n int, arrow string) string {
	return fmt.Sprintf("%s %d more", arrow, n)
}
