package code

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

// Paste payloads are owned by ranges, never by matching the placeholder's text.
// This lets users paste a literal label without accidentally expanding it.
type editorPaste struct {
	start, end int
	text       string
}

func (e *Editor) InsertPaste(text string) {
	chars := utf8.RuneCountInString(text)
	if chars < 1000 && strings.Count(text, "\n") < 8 {
		e.Insert(text)
		return
	}
	e.pasteID++
	label := fmt.Sprintf("[Paste %d · %d chars]", e.pasteID, chars)
	e.Insert(label)
	e.pastes = append(e.pastes, editorPaste{
		start: e.cursor - utf8.RuneCountInString(label), end: e.cursor, text: text,
	})
	slices.SortFunc(e.pastes, func(a, b editorPaste) int { return a.start - b.start })
}

func (e *Editor) pasteRange(from, to int) (int, int) {
	for _, paste := range e.pastes {
		if from == to && from > paste.start && from < paste.end {
			return paste.end, paste.end
		}
		if from < paste.end && to > paste.start {
			from = min(from, paste.start)
			to = max(to, paste.end)
		}
	}
	return from, to
}

func (e *Editor) shiftPastes(from, to, inserted int) {
	remaining := e.pastes[:0]
	for _, paste := range e.pastes {
		switch {
		case paste.end <= from:
			remaining = append(remaining, paste)
		case paste.start >= to:
			paste.start += inserted - (to - from)
			paste.end += inserted - (to - from)
			remaining = append(remaining, paste)
		}
	}
	clear(e.pastes[len(remaining):])
	e.pastes = remaining
}

func (e *Editor) snapPasteCursor(backward bool) {
	for _, paste := range e.pastes {
		if e.cursor > paste.start && e.cursor < paste.end {
			e.cursor = paste.end
			if backward {
				e.cursor = paste.start
			}
			return
		}
	}
}

func (e *Editor) expandPastes() bool {
	if len(e.pastes) == 0 {
		return false
	}
	cursor := e.cursor
	for _, paste := range e.pastes {
		if paste.end <= e.cursor {
			cursor += utf8.RuneCountInString(paste.text) - (paste.end - paste.start)
		}
	}
	e.SetText(e.Text())
	e.cursor = cursor
	return true
}

func (e *Editor) renderPasteRow(row editorRow) string {
	var line strings.Builder
	start, end := row.start, row.start+row.runeCount
	for _, paste := range e.pastes {
		if paste.start >= end || paste.end <= start {
			continue
		}
		left, right := max(start, paste.start), min(end, paste.end)
		line.WriteString(string(e.value[start:left]))
		line.WriteString(colored(theme.Default.Cyan, string(e.value[left:right])))
		start = right
	}
	line.WriteString(string(e.value[start:end]))
	return line.String()
}
