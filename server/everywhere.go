package server

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// everywhereTTL is how long the host reuses an agent's list of every folder's
// sessions: Claude's reads each transcript, and a session the host creates or
// deletes is listed as the host holds it, not from the list.
const everywhereTTL = 30 * time.Second

type everywhereListing struct {
	at   time.Time
	list func() ([]code.SessionInfo, error)
}

// listEverywhere lists the sessions of every folder of each started agent that
// can, as its CLI's resume list does, by provider. The agents are asked
// together: each reads its own store.
func (h *Host) listEverywhere(agents []types.AgentInfo) map[string][]code.SessionInfo {
	out := map[string][]code.SessionInfo{}
	var mu sync.Mutex
	var asked sync.WaitGroup
	for _, info := range agents {
		asked.Go(func() {
			if sessions, ok := h.everywhereOf(info.Provider); ok {
				mu.Lock()
				out[info.Provider] = sessions
				mu.Unlock()
			}
		})
	}
	asked.Wait()
	return out
}

// everywhereOf lists the sessions of every folder of one agent; ok is false
// for an agent that keeps no such list, is not started, or failed to list.
func (h *Host) everywhereOf(provider string) (sessions []code.SessionInfo, ok bool) {
	// A host of one folder lists that folder's sessions, project by project.
	if h.folder != "" {
		return nil, false
	}
	b := startedBackend(h.loaded(), provider)
	if b == nil {
		return nil, false
	}
	lister, ok := everyFolderLister(b.agent)
	if !ok {
		return nil, false
	}
	h.everywhereMu.Lock()
	entry := h.everywhere[provider]
	if entry == nil || !entry.at.IsZero() && time.Since(entry.at) >= everywhereTTL {
		entry = &everywhereListing{list: sync.OnceValues(func() ([]code.SessionInfo, error) {
			return listSessionFolders(b.Project, lister)
		})}
		h.everywhere[provider] = entry
	}
	h.everywhereMu.Unlock()
	sessions, err := entry.list()
	h.everywhereMu.Lock()
	defer h.everywhereMu.Unlock()
	// A deletion during the listing invalidates the in-flight result too.
	if h.everywhere[provider] != entry {
		return nil, false
	}
	if err != nil {
		delete(h.everywhere, provider)
		return nil, false
	}
	if entry.at.IsZero() {
		entry.at = time.Now()
	}
	return sessions, true
}

// everyFolderLister is an agent's list of every folder's sessions, when it
// keeps one. An agent that does not has its sessions listed by each project.
func everyFolderLister(agent code.Agent) (code.AllSessionsLister, bool) {
	lister, ok := agent.(code.AllSessionsLister)
	return lister, ok && lister.ListsAllSessions()
}

func listSessionFolders(p *Project, lister code.AllSessionsLister) ([]code.SessionInfo, error) {
	if !p.beginOperation() {
		return nil, errWorkspaceClosed
	}
	defer p.operations.Done()
	sessions, err := lister.ListAllSessions(p.ctx)
	if err != nil {
		return nil, err
	}
	// Resolve each folder once, outside the host's publication lock.
	dirs := map[string]string{}
	listed := make([]code.SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		if !filepath.IsAbs(session.Dir) {
			continue
		}
		dir, ok := dirs[session.Dir]
		if !ok {
			dir = canonicalPath(session.Dir)
			dirs[session.Dir] = dir
		}
		session.Dir = dir
		listed = append(listed, session)
	}
	return listed, nil
}

// forgetEverywhere drops the cached lists, once the host deleted a session.
func (h *Host) forgetEverywhere() {
	h.everywhereMu.Lock()
	clear(h.everywhere)
	h.everywhereMu.Unlock()
}
