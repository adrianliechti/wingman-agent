package claw

import (
	"fmt"

	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (t *TUI) selected() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.selectedAgent
}

func (t *TUI) agentAt(index int) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if index < 0 || index >= len(t.agentNames) {
		return ""
	}

	return t.agentNames[index]
}

func (t *TUI) refreshAgents() {
	th := theme.Default

	agents, err := t.claw.ListAgents()
	if err != nil {
		return
	}

	t.mu.Lock()
	t.agentNames = agents
	t.mu.Unlock()

	current := t.agentList.GetCurrentItem()
	t.agentList.Clear()

	for _, name := range agents {
		label := "  " + name

		if t.isBusy(name) {
			label += fmt.Sprintf(" [%s]\u2026[-]", th.Yellow)
		}

		t.agentList.AddItem(label, "", 0, nil)
	}

	if current >= 0 && current < t.agentList.GetItemCount() {
		t.agentList.SetCurrentItem(current)
	}
}

func (t *TUI) selectAgent(name string) {
	t.mu.Lock()
	t.selectedAgent = name
	t.mu.Unlock()

	t.chatView.Clear()
	t.renderedCount = 0

	if messages, _, ok := t.claw.AgentState(name); ok {
		t.renderMessages(messages)
		t.renderedCount = len(messages)
	}

	t.refreshTasks()
	t.updateStatusBar()
}
