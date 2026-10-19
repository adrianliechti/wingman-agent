package code

import "time"

// Input and completion use the last complete catalog. Discovery can scan many
// directories, so refresh it off the UI loop and apply completion on that loop.
func (a *App) requestSkillRefresh() {
	if a.ctx == nil || a.ctx.Err() != nil || time.Since(a.skillRefreshAt) < time.Second || a.skillRefreshPending {
		return
	}
	a.skillRefreshPending = true
	a.skillRefreshAt = time.Now()
	workspace := a.agent.Workspace()
	go func() {
		changed := workspace.RefreshSkills()
		a.post(func() {
			a.skillRefreshPending = false
			if changed {
				a.metadataPending.Store(true)
				a.refreshMetadata()
				a.invalidate()
			}
		})
	}()
}
