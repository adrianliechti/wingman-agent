package code

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// fillRow paints the given rectangle with spaces in the default style. Used
// as a Box DrawFunc to clear an area so underlying content doesn't bleed
// through (status spacers, bottom bars, etc.).
func fillRow(screen tcell.Screen, x, y, width, height int) (int, int, int, int) {
	for row := y; row < y+height; row++ {
		for col := x; col < x+width; col++ {
			screen.SetContent(col, row, ' ', nil, tcell.StyleDefault)
		}
	}
	return x, y, width, height
}

// verticalSeparator returns a 1-column tview.Box that draws a vertical line
// in the given color, used between two-panel modal layouts.
func verticalSeparator(color tcell.Color) *tview.Box {
	box := tview.NewBox().SetBackgroundColor(tcell.ColorDefault)
	box.SetDrawFunc(func(screen tcell.Screen, x, y, width, height int) (int, int, int, int) {
		for i := y; i < y+height; i++ {
			screen.SetContent(x, i, '│', nil, tcell.StyleDefault.Foreground(color))
		}
		return x + 1, y, width - 1, height
	})
	return box
}
