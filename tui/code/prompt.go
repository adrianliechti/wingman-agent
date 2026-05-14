package code

import (
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func (a *App) currentInstructions() string {
	data := a.agent.InstructionsData()
	data.PlanMode = a.currentMode == ModePlan

	return code.BuildInstructions(data)
}
