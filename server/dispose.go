package server

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// errOpened says that a session is not one to act on without loading it: a
// client has it open, its creation failed, or its URI names no folder.
var errOpened = errors.New("session is open")

// unopened finds a session no client has open, with its forks, without
// loading it: as the host saved it, or, for one made in an agent's own CLI, as
// its agent lists it in a folder the host serves. Loading would open the
// session's project and resume its agent's thread, and its folder may be
// gone. The caller holds the session's creation gate.
func (h *Host) unopened(resource string) (alias sessionAlias, forks []sessionAlias, err error) {
	h.mu.Lock()
	_, open := h.sessions[resource]
	alias, saved := h.aliases[resource]
	forks = h.forksLocked(resource)
	h.mu.Unlock()
	if open || saved && alias.CreationError != "" {
		return sessionAlias{}, nil, errOpened
	}
	if saved {
		return alias, forks, nil
	}
	backend, id, dir, err := parseCanonicalURI(resource)
	if err != nil || dir == "" {
		return sessionAlias{}, nil, errOpened
	}
	dir = canonicalPath(dir)
	if !filepath.IsAbs(dir) || !h.serves(dir) {
		return sessionAlias{}, nil, &ahp.Error{Code: int(types.ErrorCodePermissionDenied), Message: "this host does not serve " + dir}
	}
	if resource != canonicalSessionURI(backend, id, dir) {
		return sessionAlias{}, nil, errUnknownSession
	}
	h.mu.Lock()
	other, mapped := h.savedIndexLocked()[nativeSessionKey(dir, backend, id)]
	h.mu.Unlock()
	if mapped {
		return sessionAlias{}, nil, moved(other.Resource)
	}
	// External agents delete by ID across folders. Only their own listing
	// can establish which project an unaliased native session belongs to.
	info, err := h.listedSession(backend, id, dir)
	if err != nil {
		return sessionAlias{}, nil, err
	}
	return sessionAlias{Project: dir, Backend: backend, Session: id, Title: info.Title, Created: info.UpdatedAt, Modified: info.UpdatedAt}, forks, nil
}

// disposeUnopened deletes a session that no client has open without loading
// it, and reports whether it did.
func (h *Host) disposeUnopened(resource string) (bool, error) {
	if !ahp.ValidSessionURI(resource) {
		return false, nil
	}
	defer h.creation(resource)()
	alias, forks, err := h.unopened(resource)
	if errors.Is(err, errOpened) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	for _, native := range append(forks, alias) {
		if err := h.deleteNative(native); err != nil {
			return true, err
		}
	}
	return true, h.forgetSession(resource)
}

// listedSession finds a native session of a folder as its agent lists it.
func (h *Host) listedSession(backend, id, dir string) (code.SessionInfo, error) {
	sessions, everywhere := h.everywhereOf(backend)
	if !everywhere {
		p, err := h.projectAt(dir)
		if err != nil {
			return code.SessionInfo{}, err
		}
		b, err := p.backend(backend)
		if err != nil {
			return code.SessionInfo{}, err
		}
		if sessions, err = b.listSessions(); err != nil {
			return code.SessionInfo{}, err
		}
	}
	// An agent asked for one folder need not name it again.
	if i := slices.IndexFunc(sessions, func(s code.SessionInfo) bool {
		return s.ID == id && (s.Dir == "" && !everywhere || canonicalPath(s.Dir) == dir)
	}); i >= 0 {
		return sessions[i], nil
	}
	return code.SessionInfo{}, errUnknownSession
}

// deleteNative deletes an agent's session by its id. Wingman's own keeps its
// sessions by folder, so one of a project that is not open is removed from
// where it kept them, and its folder may be gone.
func (h *Host) deleteNative(alias sessionAlias) error {
	backend, id, dir := alias.Backend, alias.Session, canonicalPath(alias.Project)
	h.mu.Lock()
	p := h.projects[dir]
	h.mu.Unlock()
	var err error
	if p == nil && backend == code.BuiltinAgentName {
		var store string
		if store, err = builtinStore(alias); err == nil {
			err = session.Delete(store, id)
		}
	} else {
		var b *backendRuntime
		if b, err = h.deleter(p, dir, backend); err != nil {
			return err
		}
		if !supportsDelete(b.agent) {
			return errors.ErrUnsupported
		}
		if !b.Project.beginOperation() {
			return errWorkspaceClosed
		}
		defer b.Project.operations.Done()
		err = b.agent.DeleteSession(b.Project.ctx, id)
	}
	// A session its agent no longer has is deleted already.
	if sessionGone(err) {
		return nil
	}
	return err
}

// deleter finds the agent that deletes a session of a folder: its project's
// when that is open; else a running one that lists every folder, which
// deletes a session wherever it was made, as any running one must once the
// folder is gone; else the one of the folder's project, opened for it.
func (h *Host) deleter(p *Project, dir, backend string) (*backendRuntime, error) {
	if p == nil {
		_, err := os.Stat(dir)
		gone := errors.Is(err, os.ErrNotExist)
		if b := startedBackend(h.loaded(), backend); b != nil {
			if _, global := everyFolderLister(b.agent); global || gone {
				return b, nil
			}
		}
		if gone {
			p, err = h.workingProject(nil)
		} else {
			p, err = h.projectAt(dir)
		}
		if err != nil {
			return nil, err
		}
	}
	return p.backend(backend)
}

// sessionGone reports an agent's answer that it does not have a session.
func sessionGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, code.ErrSessionNotFound)
}

// builtinStore is where Wingman's own agent keeps a session's journal: with
// its repository's. Aliases predating StorageRoot can outlive the .git file
// that located their journals, so such a saved ID is resolved only when
// exactly one store still contains it.
func builtinStore(alias sessionAlias) (string, error) {
	store := code.SessionsDir(cmp.Or(alias.StorageRoot, alias.Project))
	if alias.StorageRoot != "" || alias.Resource == "" {
		return store, nil
	}
	stores, err := code.SessionStores()
	if err != nil {
		return "", err
	}
	found := ""
	for _, candidate := range stores {
		ok, err := session.Exists(candidate, alias.Session)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("multiple journal stores contain session %s", alias.Session)
		}
		found = candidate
	}
	return cmp.Or(found, store), nil
}
