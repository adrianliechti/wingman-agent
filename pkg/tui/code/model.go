package code

import (
	"context"
)

func (a *App) showModelPicker() {
	available, current := a.agent.Models(a.sessionID)
	var items []PickerItem
	for _, m := range available {
		items = append(items, PickerItem{ID: m.ID, Text: m.Name})
	}
	if len(items) == 0 {
		return
	}
	a.showPicker("Select Model", items, current, func(item PickerItem) {
		_ = a.agent.SetModel(a.ctx, a.sessionID, item.ID)
		a.updateStatusBar()
	})
}

func (a *App) cycleModel() {
	go func() {
		available, current := a.agent.Models(a.sessionID)
		if len(available) <= 1 {
			return
		}
		for i, m := range available {
			if m.ID == current {
				_ = a.agent.SetModel(context.Background(), a.sessionID, available[(i+1)%len(available)].ID)
				break
			}
		}
		a.app.QueueUpdateDraw(func() {
			a.updateStatusBar()
		})
	}()
}

func (a *App) setModel(model string) {
	_ = a.agent.SetModel(a.ctx, a.sessionID, model)
}
