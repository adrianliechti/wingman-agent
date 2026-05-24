package code

func (a *App) enterPlanMode() {
	if a.currentMode == ModePlan {
		return
	}

	a.agent.SetPlanMode(a.sessionID, true)
	a.currentMode = ModePlan
	a.updateStatusBar()
}

func (a *App) exitPlanMode() {
	if a.currentMode == ModeAgent {
		return
	}

	a.agent.SetPlanMode(a.sessionID, false)
	a.currentMode = ModeAgent
	a.updateStatusBar()
}
