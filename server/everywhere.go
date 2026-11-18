package server

import (
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
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
// can, as its CLI's resume list does, by provider.
func (h *Host) listEverywhere(agents []types.AgentInfo) map[string][]code.SessionInfo {
	out := map[string][]code.SessionInfo{}
	projects := h.loaded()
	for _, info := range agents {
		b := startedBackend(projects, info.Provider)
		if b == nil {
			continue
		}
		lister, ok := everyFolderLister(b.agent)
		if !ok {
			continue
		}
		h.everywhereMu.Lock()
		entry := h.everywhere[info.Provider]
		if entry == nil || !entry.at.IsZero() && time.Since(entry.at) >= everywhereTTL {
			entry = &everywhereListing{list: sync.OnceValues(func() ([]code.SessionInfo, error) {
				return listSessionFolders(b.Project, lister)
			})}
			h.everywhere[info.Provider] = entry
		}
		h.everywhereMu.Unlock()
		sessions, err := entry.list()
		h.everywhereMu.Lock()
		// A deletion during the listing invalidates the in-flight result too.
		if h.everywhere[info.Provider] == entry {
			if err != nil {
				delete(h.everywhere, info.Provider)
			} else {
				if entry.at.IsZero() {
					entry.at = time.Now()
				}
				out[info.Provider] = sessions
			}
		}
		h.everywhereMu.Unlock()
	}
	return out
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

// everywhereSessions describes the sessions agents listed for every folder,
// each in its own folder. A folder outside the host's roots cannot open, so
// its sessions are left out.
func (h *Host) everywhereSessions(everywhere map[string][]code.SessionInfo, seen map[string]bool) []types.SessionSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	items := []types.SessionSummary{}
	saved := h.savedIndexLocked()
	for _, provider := range slices.Sorted(maps.Keys(everywhere)) {
		for _, session := range everywhere[provider] {
			dir := session.Dir
			if !h.servesLocked(dir) {
				continue
			}
			if h.listed == nil {
				h.listed = map[string]bool{}
			}
			h.listed[dir] = true
			project, directory := projectInfo(dir), fileuri.FromPath(dir)
			// A folder's open project names it as its windows do.
			if p := h.projects[dir]; p != nil {
				project, directory = p.info(), p.directoryURI()
			}
			key := nativeKey{dir, provider, session.ID}
			if summary, ok := h.listedSummaryLocked(dir, project, directory, key, session, saved, seen); ok {
				items = append(items, summary)
			}
		}
	}
	return items
}
