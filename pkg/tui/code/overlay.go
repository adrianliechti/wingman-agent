package code

import (
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
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
