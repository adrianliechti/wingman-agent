package code

import (
	"context"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestCtrlPOpensGroupedCommandCenterAndCtrlKStillEdits(t *testing.T) {
	a := &App{ctx: context.Background(), agent: newUITestAgent(nil), editor: NewEditor()}
	a.editor.SetText("keep remove")
	a.editor.cursor = 4
	a.handleKey(inline.KeyEvent{Key: inline.KeyCtrl, Rune: 'k'})
	if got := a.editor.Text(); got != "keep" {
		t.Fatalf("ctrl+k text = %q", got)
	}

	a.handleKey(inline.KeyEvent{Key: inline.KeyCtrl, Rune: 'p'})
	if a.popup == nil || a.popup.kind != popupPalette {
		t.Fatal("ctrl+p did not open command center")
	}
	groups := map[string]bool{}
	for _, item := range a.popup.items {
		groups[item.Group] = true
	}
	for _, want := range []string{"Workspace", "Session", "Agent", "Application"} {
		if !groups[want] {
			t.Errorf("missing %s group", want)
		}
	}
	if groups["Appearance"] {
		t.Error("removed theme feature left an Appearance group")
	}
}
