package code

func (a *App) enterPlanMode() {
	if a.currentMode == ModePlan {
		return
	}

	_ = a.agent.SetMode(a.ctx, a.sessionID, "plan")
	a.currentMode = ModePlan
	a.updateStatusBar()
}

func (a *App) exitPlanMode() {
	if a.currentMode == ModeAgent {
		return
	}

	_ = a.agent.SetMode(a.ctx, a.sessionID, "agent")
	a.currentMode = ModeAgent
	a.updateStatusBar()
}
