package server

func (s *Project) hasClients() bool {
	return s != nil && s.host != nil && s.host.connected()
}

func (s *Project) flushFiles() {
	if s.fileMonitor != nil {
		s.fileMonitor.Flush()
	}
	s.workspaceChanged(Frame{Type: EvtFilesChanged})
}

func (s *Project) checkWorkspace() {
	ws := s.workspace

	gitNow := ws.IsGitRepo()
	if gitNow != s.prevGit {
		ws.SyncProjectMode()
		s.workspaceChanged(Frame{Type: EvtWorkspaceChanged})
		s.prevGit = gitNow
	}

	if !ws.HasChanges() {
		s.workspaceChanged(Frame{Type: EvtFilesChanged})
		return
	}
	fp := ws.ChangesFingerprint(s.ctx)
	if fp != s.prevFingerprint {
		s.prevFingerprint = fp
		s.workspaceChanged(Frame{Type: EvtFilesChanged})
	}
}

func (s *Project) refreshSkills() {
	if s.workspace.RefreshSkills() {
		s.workspaceChanged(Frame{Type: EvtSkillsChanged})
	}
}
