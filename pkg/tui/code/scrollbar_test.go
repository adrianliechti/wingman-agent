package code

import (
	"fmt"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func assertRightEdge(t *testing.T, line string, width int, suffix string) {
	t.Helper()
	if ansi.Width(line) != width || !strings.HasSuffix(ansi.Strip(line), suffix) {
		t.Fatalf("want %q at column %d, got width %d: %q", suffix, width, ansi.Width(line), ansi.Strip(line))
	}
}

func TestScrollbarReachesBothEnds(t *testing.T) {
	for _, total := range []int{11, 30, 1000} {
		assertRightEdge(t, scrollbarLine(strings.Repeat("界", 30), 40, scrollMarker(0, 10, 0, total)), 40, "┃")
		assertRightEdge(t, scrollbarLine("last", 40, scrollMarker(9, 10, total-10, total)), 40, "┃")
		assertRightEdge(t, scrollbarLine("", 40, scrollMarker(9, 10, 0, total)), 40, "│")
	}
	assertRightEdge(t, scrollbarLine("fits", 40, scrollMarker(0, 10, 0, 2)), 40, " ")
	assertRightEdge(t, closeHeader(strings.Repeat("界", 30), 40), 40, "×")
}

func TestPopupScrollbarAndCloseAtRightEdge(t *testing.T) {
	items := make([]PopupItem, 20)
	for i := range items {
		items[i] = PopupItem{ID: fmt.Sprint(i), Label: strings.Repeat("choice ", 20)}
	}
	for _, kind := range []popupKind{popupList, popupFiles, popupCommands, popupPalette} {
		for _, width := range []int{40, 120} {
			p := newPopup(kind, "choices", items, nil)
			p.maxRows = 4
			lines := p.Render(width)
			assertRightEdge(t, lines[0], width, "×")
			assertRightEdge(t, lines[1], width, "┃")
			assertRightEdge(t, lines[4], width, "│")
			p.SelectID("19")
			lines = p.Render(width)
			assertRightEdge(t, lines[1], width, "│")
			assertRightEdge(t, lines[4], width, "┃")
		}
	}
}

func TestDiffScrollbarIncludesPinnedRowsAndFinalLine(t *testing.T) {
	p := &diffPanel{diffs: diffPanelFixture}
	p.render(70, 8)
	p.offset = p.sections[0].nameRow + 1
	lines := p.render(70, 8)
	assertRightEdge(t, lines[0], 70, "×")
	for _, line := range lines[1:] {
		plain := ansi.Strip(line)
		if ansi.Width(line) != 70 || !(strings.HasSuffix(plain, "│") || strings.HasSuffix(plain, "┃")) {
			t.Fatalf("pinned panel row lacks right-edge track: %q", plain)
		}
	}
	p.offset = len(p.lines)
	lines = p.render(70, 8)
	last := lines[len(lines)-1]
	assertRightEdge(t, last, 70, "┃")
	if want := strings.TrimSpace(ansi.Strip(p.lines[len(p.lines)-1])); !strings.Contains(ansi.Strip(last), want) {
		t.Fatalf("final diff line is unreachable: want %q in %q", want, ansi.Strip(last))
	}
}

func TestOverlayScrollbarAtRightEdge(t *testing.T) {
	content := make([]string, 40)
	for i := range content {
		content[i] = strings.Repeat("line ", 50)
	}
	for _, width := range []int{40, 120} {
		o := newTwoPaneOverlay("changes", "1 file", 1,
			func(bool, int) string { return "file.go" },
			func(int) []string { return content }, nil)
		o.detailView = true
		lines := o.Render(width, 12)
		assertRightEdge(t, lines[0], width, "×")
		assertRightEdge(t, lines[2], width, "┃")
		assertRightEdge(t, lines[10], width, "│")
		o.scrollDetail(len(content))
		lines = o.Render(width, 12)
		assertRightEdge(t, lines[10], width, "┃")
	}
	p := pager{title: "agent", lines: content}
	lines := p.Render(80, 12)
	assertRightEdge(t, lines[0], 80, "×")
	assertRightEdge(t, lines[2], 80, "┃")
}

func TestMouseClosesOverlaysAndPickersAtFinalColumn(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 80, 20)
	a.showTranscript()
	a.render()
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: 80, Y: 2})
	if a.overlay == nil {
		t.Fatal("click below close button closed overlay")
	}
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: 80, Y: 1})
	if a.overlay != nil {
		t.Fatal("last-column close button did not close overlay")
	}
	for _, kind := range []popupKind{popupList, popupPalette, popupCommands} {
		cancelled := false
		a.popup = newPopup(kind, "choices", []PopupItem{{ID: "one", Label: "one"}}, nil)
		a.popup.onCancel = func() { cancelled = true }
		a.render()
		y := a.mouse.popupStart + a.popup.closeRow + 1
		a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: 80, Y: y})
		if a.popup != nil || !cancelled {
			t.Fatal("picker close button did not cancel picker")
		}
	}
}
