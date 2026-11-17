package server

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// disposeUnopened deletes a session that no client has open without loading
// it, and reports whether it did: its folder may be gone, and an agent such as
// Codex would resume a whole thread to load it.
func (h *Host) disposeUnopened(resource string) (bool, error) {
	if !ahp.ValidSessionURI(resource) {
		return false, nil
	}
	defer h.creation(resource)()
	h.mu.Lock()
	if h.sessions[resource] != nil {
		h.mu.Unlock()
		return false, nil
	}
	alias, saved := h.aliases[resource]
	forks := h.forksLocked(resource)
	h.mu.Unlock()
	if saved && alias.CreationError != "" {
		return false, nil
	}
	if !saved {
		backend, id, dir, err := parseCanonicalURI(resource)
		if err != nil || dir == "" {
			return false, nil
		}
		dir = canonicalPath(dir)
		if !filepath.IsAbs(dir) || !h.serves(dir) {
			return true, &ahp.Error{Code: int(types.ErrorCodePermissionDenied), Message: "project outside host roots"}
		}
		if resource != canonicalSessionURI(backend, id, dir) {
			return true, errUnknownSession
		}
		key := nativeSessionKey(dir, backend, id)
		h.mu.Lock()
		other, mapped := h.savedIndexLocked()[key]
		h.mu.Unlock()
		if mapped {
			return true, moved(other.Resource)
		}
		// External agents delete by ID across folders. Only their own listing
		// can establish which project an unaliased native session belongs to.
		if err := h.checkListedSession(backend, id, dir); err != nil {
			return true, err
		}
		alias = sessionAlias{Project: dir, Backend: backend, Session: id}
	}
	for _, fork := range forks {
		if err := h.deleteNative(fork); err != nil {
			return true, err
		}
	}
	if err := h.deleteNative(alias); err != nil {
		return true, err
	}
	return true, h.forgetSession(resource, alias, forks)
}

func (h *Host) checkListedSession(backend, id, dir string) error {
	if sessions, ok := h.listEverywhere([]types.AgentInfo{{Provider: backend}})[backend]; ok {
		if slices.ContainsFunc(sessions, func(s code.SessionInfo) bool {
			return s.ID == id && s.Dir == dir
		}) {
			return nil
		}
		return errUnknownSession
	}
	p, err := h.projectAt(dir)
	if err != nil {
		return err
	}
	b, err := p.backend(backend)
	if err != nil {
		return err
	}
	if !p.beginOperation() {
		return errWorkspaceClosed
	}
	defer p.operations.Done()
	sessions, err := b.agent.ListSessions(p.ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(sessions, func(s code.SessionInfo) bool {
		return s.ID == id && (s.Dir == "" || canonicalPath(s.Dir) == dir)
	}) {
		return nil
	}
	return errUnknownSession
}

// deleteNative deletes an agent's session by its id. An ACP agent deletes it
// wherever it was made; Wingman's own keeps sessions by folder, and those of
// a folder that is gone are removed from where it kept them.
func (h *Host) deleteNative(alias sessionAlias) error {
	backend, id, dir := alias.Backend, alias.Session, canonicalPath(alias.Project)
	h.mu.Lock()
	p := h.projects[dir]
	h.mu.Unlock()
	var b *backendRuntime
	if p == nil {
		_, statErr := os.Stat(dir)
		gone := errors.Is(statErr, os.ErrNotExist)
		if backend == code.BuiltinAgentName && gone {
			store, err := removedProjectStore(alias)
			if err != nil {
				return err
			}
			return session.Delete(store, id)
		}
		// A running global provider can also serve a closed or removed folder.
		if backend != code.BuiltinAgentName {
			b = startedBackend(h.loaded(), backend)
			if b != nil && !gone {
				if _, global := b.agent.(code.AllSessionsLister); !global {
					b = nil
				}
			}
		}
		if b == nil {
			var err error
			if gone {
				p, err = h.workingProject(nil)
			} else {
				p, err = h.projectAt(dir)
			}
			if err != nil {
				return err
			}
		}
	}
	if b == nil {
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

// Aliases predating StorageRoot can outlive the .git file that located their
// journals. Resolve a saved ID only when exactly one store still contains it.
func removedProjectStore(alias sessionAlias) (string, error) {
	store := code.SessionsDir(cmp.Or(alias.StorageRoot, alias.Project))
	if alias.StorageRoot != "" || alias.Resource == "" {
		return store, nil
	}
	if _, err := session.ArtifactDir(store, alias.Session); err != nil {
		return "", err
	}
	projects, err := layout.WingmanPath("projects")
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(projects)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	found := ""
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(projects, entry.Name(), "sessions")
		for _, suffix := range []string{".jsonl", ".json", ".data"} {
			_, err := os.Lstat(filepath.Join(candidate, alias.Session+suffix))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return "", err
			}
			if found != "" {
				return "", fmt.Errorf("multiple journal stores contain session %s", alias.Session)
			}
			found = candidate
			break
		}
	}
	return cmp.Or(found, store), nil
}

// forgetSession drops a deleted session and its forks from the host and from
// disk, through its folder's project while that is open, and tells clients.
func (h *Host) forgetSession(resource string, alias sessionAlias, forks []sessionAlias) error {
	// A project opening concurrently must read its metadata after deletion.
	h.projectMu.Lock()
	defer h.projectMu.Unlock()
	h.mu.Lock()
	p := h.projects[alias.Project]
	if p == nil {
		h.mu.Unlock()
		for _, gone := range append(forks, alias) {
			if gone.Resource != "" {
				if err := removeAlias(workspaceScope(gone.Project), gone); err != nil {
					return err
				}
			}
		}
		h.mu.Lock()
		for _, fork := range forks {
			delete(h.chatAliases, fork.Chat)
		}
		delete(h.aliases, resource)
	} else {
		for _, fork := range forks {
			p.forgetChatAlias(fork.Chat)
		}
		p.forgetAlias(resource)
	}
	h.forgetEverywhere()
	h.notifyRoot("root/sessionRemoved", types.SessionRemovedParams{Channel: types.RootResourceURI, Session: resource})
	h.mu.Unlock()
	if p != nil {
		return p.flushAliases()
	}
	return nil
}
