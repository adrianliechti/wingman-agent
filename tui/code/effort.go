package code

func (a *App) showEffortPicker() {
	current, options := a.agent.Effort()
	if len(options) == 0 {
		return
	}
	items := make([]PickerItem, 0, len(options))
	for _, v := range options {
		items = append(items, PickerItem{ID: v, Text: titleCase(v)})
	}
	a.showPicker("Select Effort", items, current, func(item PickerItem) {
		a.setEffort(item.ID)
		a.updateStatusBar()
	})
}

func (a *App) setEffort(effort string) {
	_ = a.agent.SetEffort(a.ctx, effort)
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}
