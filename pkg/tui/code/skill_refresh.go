package code

import "time"

// Input and completion use the last complete catalog. Discovery can scan many
// directories, so refresh it off the UI loop and publish changes atomically.
func (a *App) requestSkillRefresh() {
	if a.ctx == nil || a.ctx.Err() != nil || time.Since(a.skillRefreshAt) < time.Second || a.skillRefreshPending.Swap(true) {
		return
	}
	a.skillRefreshAt = time.Now()
	workspace := a.agent.Workspace()
	go func() {
		defer a.skillRefreshPending.Store(false)
		if workspace.RefreshSkills() {
			a.metadataPending.Store(true)
			a.requestRender()
		}
	}()
}
