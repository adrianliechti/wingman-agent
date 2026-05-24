package code

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func fillRow(screen tcell.Screen, x, y, width, height int) (int, int, int, int) {
	for row := y; row < y+height; row++ {
		for col := x; col < x+width; col++ {
			screen.SetContent(col, row, ' ', nil, tcell.StyleDefault)
		}
	}
	return x, y, width, height
}

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
