package server

import (
	"errors"
	"os"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// disposeUnopened deletes a session that no client has open without loading
// it, and reports whether it did: its folder may be gone, and an agent such as
// Codex would resume a whole thread to load it.
func (h *Host) disposeUnopened(channel string) (bool, error) {
	h.mu.Lock()
	resource := h.sessionURI(channel)
	if channel != resource || h.sessions[resource] != nil {
		h.mu.Unlock()
		return false, nil
	}
	alias, saved := h.aliases[resource]
	forks := h.forksLocked(resource)
	h.mu.Unlock()
	if saved && alias.CreationError != "" {
		return false, nil
	}
	backend, id, dir := alias.Backend, alias.Session, alias.Project
	if !saved {
		var err error
		if backend, id, dir, err = parseCanonicalURI(resource); err != nil || dir == "" {
			return false, nil
		}
	}
	for _, fork := range forks {
		if err := h.deleteNative(fork.Backend, fork.Session, fork.Project); err != nil {
			return true, err
		}
	}
	if err := h.deleteNative(backend, id, dir); err != nil {
		return true, err
	}
	h.forgetSession(resource)
	return true, nil
}

// deleteNative deletes an agent's session by its id. An ACP agent deletes it
// wherever it was made; Wingman's own keeps sessions by folder, and those of
// a folder that is gone are removed from where it kept them.
func (h *Host) deleteNative(backend, id, dir string) error {
	dir = canonicalPath(dir)
	var b *backendRuntime
	if backend == code.BuiltinAgentName {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			if err := session.Delete(code.SessionsDir(dir), id); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		p, err := h.projectAt(dir)
		if err != nil {
			return err
		}
		if b, err = p.backend(backend); err != nil {
			return err
		}
	} else if b = h.startedBackend(backend); b == nil {
		p := h.webProject()
		if p == nil {
			return errWorkspaceClosed
		}
		var err error
		if b, err = p.backend(backend); err != nil {
			return err
		}
	}
	if !supportsDelete(b.agent) {
		return errors.ErrUnsupported
	}
	if !b.Project.beginOperation() {
		return errWorkspaceClosed
	}
	defer b.Project.operations.Done()
	err := b.agent.DeleteSession(b.Project.ctx, id)
	// A session its agent no longer has is deleted already.
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, code.ErrSessionNotFound) {
		return nil
	}
	return err
}

// forgetSession drops a deleted session and its forks from the host and from
// disk, through its folder's project while that is open, and tells clients.
func (h *Host) forgetSession(resource string) {
	h.mu.Lock()
	alias, saved := h.aliases[resource]
	forks := h.forksLocked(resource)
	p := h.projects[alias.Project]
	if p != nil {
		for _, fork := range forks {
			p.forgetChatAlias(fork.Chat)
		}
		p.forgetAlias(resource)
	} else {
		for _, fork := range forks {
			delete(h.chatAliases, fork.Chat)
		}
		delete(h.aliases, resource)
	}
	h.notifyRoot("root/sessionRemoved", types.SessionRemovedParams{Channel: types.RootResourceURI, Session: resource})
	h.mu.Unlock()
	h.forgetEverywhere()
	switch {
	case p != nil:
		p.saveAliases()
	case saved:
		for _, gone := range append(forks, alias) {
			removeAliasFile(gone)
		}
	}
}

// removeAliasFile removes a saved session or chat of a folder no project shows.
func removeAliasFile(alias sessionAlias) {
	dir, err := layout.WingmanPath("ahp", workspaceScope(alias.Project).WorkspaceID)
	if err != nil {
		return
	}
	_ = os.Remove(aliasPath(dir, alias))
}
