package code

import (
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestPopupRecoversAfterEmptySearchAndCanAcceptEmptyMulti(t *testing.T) {
	p := newPopup(popupPalette, "commands", []PopupItem{{ID: "one", Label: "one"}}, nil)
	p.SetQuery("missing")
	p.HandleKey(inline.KeyEvent{Key: inline.KeyPgDn})
	p.SetQuery("")
	if item, ok := p.Current(); !ok || item.ID != "one" {
		t.Fatalf("popup did not recover current item: %+v, %v", item, ok)
	}

	accepted := false
	p = newPopup(popupList, "context", []PopupItem{{ID: "one", Label: "one"}}, func(ids []string) {
		accepted = len(ids) == 0
	})
	p.multi = true
	p.acceptEmpty = true
	_, closed := p.HandleKey(inline.KeyEvent{Key: inline.KeyEnter})
	if !closed || !accepted {
		t.Fatal("empty multi-selection was not applied")
	}
}
