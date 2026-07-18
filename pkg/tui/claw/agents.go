package claw

func (t *TUI) refreshAgents() {
	agents, err := t.claw.ListAgents()
	if err != nil {
		return
	}

	t.mu.Lock()
	t.agentNames = agents
	t.mu.Unlock()

	if t.agentIndex >= len(agents) {
		t.agentIndex = 0
	}
}
