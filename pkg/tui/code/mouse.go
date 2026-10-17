package code

import "github.com/adrianliechti/wingman-agent/pkg/tui/inline"

// Mouse reports are one-based; rendered hit areas use zero-based cells.
type mouseRect struct {
	x, y, width, height int
}

func (r mouseRect) contains(x, y int) bool {
	return x >= r.x && x < r.x+r.width && y >= r.y && y < r.y+r.height
}

type mouseLayout struct {
	model       mouseRect
	popup       *Popup
	popupBounds mouseRect
	popupStart  int
	footer      []mouseAction
}

type mouseAction struct {
	bounds mouseRect
	key    inline.KeyEvent
}

func (a *App) handleMouseControls(ev inline.MouseEvent) bool {
	x, y := ev.X-1, ev.Y-1
	p := &a.diffPanel
	if p.resizing {
		switch ev.Kind {
		case inline.MouseDrag, inline.MouseRelease:
			w, _ := a.term.Size()
			if a.diffPanelWidth(w) > 0 {
				width := min(max(diffPanelMinWidth, p.resizeWidth+p.resizeX-x), w-diffPanelChatMinWidth-2)
				if width != a.diffPanelWidth(w) {
					p.width = width
					a.rebuildChat()
				}
			}
			if ev.Kind == inline.MouseRelease {
				p.resizing = false
			}
		}
		return true
	}

	if a.diffPanelShowing {
		w, h := a.term.Size()
		panelWidth := a.diffPanelWidth(w)
		if panelWidth > 0 && y >= 0 && y < h {
			if ev.Kind == inline.MousePress && y == 0 && x >= w-closeButtonWidth && x < w {
				a.clearSelection()
				a.toggleDiffPanel()
				return true
			}
			chatWidth := w - panelWidth - 2
			if ev.Kind == inline.MousePress && x >= chatWidth && x < chatWidth+2 {
				a.clearSelection()
				p.resizing, p.resizeX, p.resizeWidth = true, x, panelWidth
				a.invalidate()
				return true
			}
		}
	}

	if popup := a.popup; popup != nil && popup == a.mouse.popup && a.mouse.popupBounds.contains(x, y) {
		switch ev.Kind {
		case inline.MouseWheel:
			popup.index = min(max(popup.index+ev.WheelDelta*3, 0), max(len(popup.filtered)-1, 0))
		case inline.MousePress:
			if y-a.mouse.popupStart == popup.closeRow && x >= a.mouse.popupBounds.width-closeButtonWidth {
				a.closePopup()
				a.invalidate()
				return true
			}
			if x == a.mouse.popupBounds.width-1 {
				// The scrollbar gutter is not an item activation target.
				return true
			}
			if index, ok := popup.mouseRows[y-a.mouse.popupStart]; ok {
				popup.index = index
				if item, ok := popup.Current(); ok && !item.Disabled {
					if popup.multi {
						popup.SetSelected(item.ID, !popup.selected[item.ID])
					} else {
						a.handlePopupKey(inline.KeyEvent{Key: inline.KeyEnter})
					}
				}
			}
		}
		a.invalidate()
		return true
	}
	if a.popup != nil && (a.popup.kind == popupList || a.popup.kind == popupPalette) {
		// Standalone pickers capture mouse input just as they capture keys.
		return true
	}
	if ev.Kind == inline.MousePress && a.popup == nil {
		for _, action := range a.mouse.footer {
			if action.bounds.contains(x, y) {
				a.clearSelection()
				// File attachment is available between turns, just like @.
				if action.key.Key != inline.KeyRune || action.key.Rune != '@' || !a.isStreaming() {
					a.handleKey(action.key)
				}
				a.invalidate()
				return true
			}
		}
	}
	if ev.Kind == inline.MousePress && a.mouse.model.contains(x, y) {
		a.clearSelection()
		a.showModelPicker()
		a.invalidate()
		return true
	}
	return false
}
