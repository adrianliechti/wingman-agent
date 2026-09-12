package code

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestCollapsedPastesPreservePayloadThroughEditsAndExpansion(t *testing.T) {
	e := NewEditor()
	first, second := strings.Repeat("日本語\n", 10), strings.Repeat("🦋", 1000)
	e.Insert("before ")
	e.InsertPaste(first)
	e.Insert(" between ")
	e.InsertPaste(second)
	e.Insert(" after [Paste 1 · 40 chars]")
	want := "before " + first + " between " + second + " after [Paste 1 · 40 chars]"
	if e.Text() != want || len(e.pastes) != 2 || len(e.value) > 150 {
		t.Fatal("paste did not collapse or altered its payload")
	}
	e.ReplaceRange(0, len("before"), "prefix")
	want = strings.Replace(want, "before", "prefix", 1)
	if e.Text() != want {
		t.Fatal("editing before a paste lost its payload")
	}
	lines, _ := e.Render(18, 20, EditorChrome{})
	for _, line := range lines {
		if ansi.Width(line) > 18 {
			t.Fatalf("narrow paste row overflowed: %q", line)
		}
	}
	if strings.Contains(strings.Join(lines, ""), first) {
		t.Fatal("render exposed the collapsed payload")
	}
	e.cursor = len(e.value)
	if !e.HandleKey(inline.KeyEvent{Key: inline.KeyRune, Rune: 'e', Alt: true}) || e.Text() != want || len(e.pastes) != 0 || e.cursor != utf8.RuneCountInString(want) {
		t.Fatal("Alt+E lost text or the logical cursor")
	}
	e.AddHistory(e.Text())
	e.SetText("")
	e.HistoryPrev()
	if e.Text() != want {
		t.Fatal("history lost the expanded paste")
	}
}

func TestCollapsedPasteIsAnAtomicEditingUnit(t *testing.T) {
	for _, key := range []inline.Key{inline.KeyBackspace, inline.KeyDelete} {
		e := NewEditor()
		e.Insert("left ")
		e.InsertPaste(strings.Repeat("x", 1000))
		e.Insert(" right")
		paste := e.pastes[0]
		e.cursor = paste.start
		if key == inline.KeyBackspace {
			e.cursor = paste.end
		}
		e.HandleKey(inline.KeyEvent{Key: key})
		if e.Text() != "left  right" || len(e.pastes) != 0 {
			t.Fatalf("key %v partially deleted a paste: %q", key, e.Text())
		}
	}
	e := NewEditor()
	e.InsertPaste(strings.Repeat("x", 1000))
	e.HandleKey(inline.KeyEvent{Key: inline.KeyLeft})
	if e.cursor != 0 {
		t.Fatal("Left landed inside a placeholder")
	}
	e.HandleKey(inline.KeyEvent{Key: inline.KeyRight})
	if e.cursor != len(e.value) {
		t.Fatal("Right landed inside a placeholder")
	}
	e.ReplaceRange(2, 4, "replacement")
	if e.Text() != "replacement" || len(e.pastes) != 0 {
		t.Fatal("partial replacement left an orphaned payload")
	}
}

func TestModifiedEnterInsertsNewlineWithoutSubmitting(t *testing.T) {
	for _, key := range []inline.KeyEvent{{Key: inline.KeyEnter, Shift: true}, {Key: inline.KeyEnter, Alt: true}, {Key: inline.KeyCtrl, Rune: 'j'}} {
		e := NewEditor()
		e.Insert("first")
		if !e.HandleKey(key) {
			t.Fatalf("newline key was not consumed: %+v", key)
		}
		e.Insert("second")
		if e.Text() != "first\nsecond" || e.HandleKey(inline.KeyEvent{Key: inline.KeyEnter}) {
			t.Fatalf("newline/submission behavior changed for %+v", key)
		}
	}
}
