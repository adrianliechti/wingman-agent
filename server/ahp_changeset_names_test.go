package server

// The web UI subscribes to a workspace server's changes under these names.
const (
	ahpWorkspaceChanges = "ahp-changeset:/wingman/uncommitted"
	ahpStagedChanges    = "ahp-changeset:/wingman/staged"
	ahpUnstagedChanges  = "ahp-changeset:/wingman/unstaged"
)
