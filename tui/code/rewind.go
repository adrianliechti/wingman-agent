package code

import (
	"fmt"

	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (a *App) showRewindPicker() {
	t := theme.Default

	checkpoints, err := a.agent.Checkpoints()
	if err != nil {
		fmt.Fprint(a.chatView, a.formatNotice("Rewind unavailable in this workspace", t.Yellow))
		return
	}
	if len(checkpoints) == 0 {
		fmt.Fprint(a.chatView, a.formatNotice("No checkpoints available", t.Yellow))
		return
	}

	items := make([]PickerItem, len(checkpoints))

	for i, cp := range checkpoints {
		items[i] = PickerItem{
			ID:   cp.Hash,
			Text: fmt.Sprintf("%s - %s", cp.Time.Format("15:04:05"), cp.Message),
		}
	}

	a.showPicker("Rewind to", items, "", func(item PickerItem) {
		if err := a.agent.Restore(item.ID); err != nil {
			fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Failed to restore: %v", err), t.Red))
			return
		}

		fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Restored to: %s", item.Text), t.Green))
	})
}

func (a *App) commitRewind(message string) {
	if len(message) > 50 {
		message = message[:50]
	}

	go func() {
		_ = a.agent.Commit(message)
	}()
}
