package code

import (
	"fmt"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func queuedText(input code.TurnInput) string {
	if input.Display != nil {
		return input.Display.Text
	}
	for _, part := range input.Content {
		if !part.Hidden && part.Text != "" {
			return part.Text
		}
	}
	return "Queued input"
}

// Restore from the turn manager, rather than reconstructing a queue from
// events that may have happened before this frontend attached.
func (a *App) syncQueuedInputs() {
	if a.turns == nil {
		return
	}
	snapshot := a.turns.Snapshot(a.sessionID)
	a.queuePaused = snapshot.Paused
	a.queueCount = 0
	var items []pendingEchoItem
	for _, item := range snapshot.Inputs {
		if item.State == code.TurnInputQueued && item.Input.Origin != "task" {
			a.queueCount++
			items = append(items, pendingEchoItem{ID: item.ID, Text: queuedText(item.Input), State: item.State})
		}
	}
	a.pendingEchoMu.Lock()
	a.pendingEcho = items
	a.pendingEchoMu.Unlock()
	if snapshot.Error != nil {
		a.showToast("Could not restore queue: "+snapshot.Error.Error(), theme.Default.Red)
	}
}

func (a *App) showTurnQueue() {
	a.syncQueuedInputs()
	if a.turns == nil {
		return
	}
	snapshot := a.turns.Snapshot(a.sessionID)
	var items []PopupItem
	if snapshot.Paused {
		items = append(items, PopupItem{ID: "resume", Label: "Resume queue", Detail: "Run saved inputs in order"})
	}
	for _, item := range snapshot.Inputs {
		if item.State != code.TurnInputQueued || item.Input.Origin == "task" {
			continue
		}
		items = append(items, PopupItem{ID: item.ID, Label: commandLine(queuedText(item.Input)), Detail: fmt.Sprintf("%d · edit or remove", item.Position)})
	}
	if a.queueCount > 0 {
		items = append(items, PopupItem{ID: "clear", Label: "Clear queue", Detail: "Remove all waiting inputs"})
	}
	if len(items) == 0 {
		a.showToast("No queued inputs · Alt+Enter queues a follow-up", theme.Default.BrBlack)
		return
	}
	title := "Queued inputs"
	if snapshot.Paused {
		title += " · paused"
	}
	a.popup = newPopup(popupList, title, items, func(ids []string) {
		id := ids[0]
		if id == "resume" || id == "clear" {
			a.runQueueCommand(id)
			return
		}
		a.popup = newPopup(popupList, "Queued input", []PopupItem{{ID: "edit", Label: "Edit input", Detail: "Keep its queue position"}, {ID: "remove", Label: "Remove input"}}, func(actions []string) { a.runQueueCommand(actions[0] + " " + id) })
	})
	a.invalidate()
}

func (a *App) runQueueCommand(args string) {
	if a.turns == nil {
		return
	}
	action, id, _ := strings.Cut(strings.TrimSpace(args), " ")
	id = strings.TrimSpace(id)
	var err error
	switch action {
	case "":
		a.showTurnQueue()
		return
	case "resume":
		a.turns.Resume(a.sessionID)
		if state := a.turns.Snapshot(a.sessionID); state.Paused {
			err = fmt.Errorf("queue could not resume; check session storage")
		}
	case "clear":
		err = a.turns.ClearQueue(a.sessionID)
	case "edit", "remove":
		var found *code.TurnInput
		for _, item := range a.turns.Snapshot(a.sessionID).Inputs {
			if id != "" && item.State == code.TurnInputQueued && item.Input.Origin != "task" && strings.HasPrefix(item.ID, id) {
				if found != nil {
					err = fmt.Errorf("queue id %q is ambiguous", id)
					break
				}
				input := code.CloneTurnInput(item.Input)
				found = &input
			}
		}
		if err == nil && found == nil {
			err = fmt.Errorf("queued input %q was not found", id)
		}
		if err == nil {
			if action == "remove" {
				err = a.turns.RemoveQueued(a.sessionID, found.ID)
			} else {
				a.editQueuedInput(*found)
			}
		}
	default:
		err = fmt.Errorf("use /queue resume, clear, edit <id>, or remove <id>")
	}
	if err != nil {
		a.showToast(err.Error(), theme.Default.Red)
	}
	a.syncQueuedInputs()
	a.invalidate()
}

func (a *App) editQueuedInput(input code.TurnInput) {
	if a.editor == nil {
		return
	}
	if a.editingQueueID == "" {
		draft := a.currentDraft()
		a.beforeQueueEdit = &draft
	}
	a.editingQueueID = input.ID
	a.editor.SetText(queuedText(input))
	a.pendingContent = nil
	for _, part := range input.Content {
		if part.File != nil {
			a.pendingContent = append(a.pendingContent, agent.CloneContent([]agent.Content{part})...)
		}
	}
	a.pendingFiles = nil
	if input.Display != nil {
		a.pendingFiles = slices.Clone(input.Display.Files)
	}
	a.showToast("Editing queued input · Enter updates · Esc cancels", theme.Default.Cyan)
}

func (a *App) endQueueEdit() {
	a.editingQueueID = ""
	if a.beforeQueueEdit != nil {
		a.restoreDraft(*a.beforeQueueEdit)
		a.beforeQueueEdit = nil
	}
}
