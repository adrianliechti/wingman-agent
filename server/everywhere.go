package server

import (
	"maps"
	"slices"
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
	at       time.Time
	sessions []code.SessionInfo
}

// listEverywhere lists the sessions of every folder of each started agent that
// can, as its CLI's resume list does, by provider.
func (h *Host) listEverywhere(agents []types.AgentInfo) map[string][]code.SessionInfo {
	out := map[string][]code.SessionInfo{}
	for _, info := range agents {
		h.everywhereMu.Lock()
		cached, ok := h.everywhere[info.Provider]
		h.everywhereMu.Unlock()
		if ok && time.Since(cached.at) < everywhereTTL {
			out[info.Provider] = cached.sessions
			continue
		}
		b := h.startedBackend(info.Provider)
		if b == nil {
			continue
		}
		lister, ok := b.agent.(code.AllSessionsLister)
		if !ok || !b.Project.beginOperation() {
			continue
		}
		sessions, err := lister.ListAllSessions(b.Project.ctx)
		b.Project.operations.Done()
		// An agent that cannot list every folder has its open projects' listed.
		if err != nil {
			continue
		}
		h.everywhereMu.Lock()
		if h.everywhere == nil {
			h.everywhere = map[string]everywhereListing{}
		}
		h.everywhere[info.Provider] = everywhereListing{at: time.Now(), sessions: sessions}
		h.everywhereMu.Unlock()
		out[info.Provider] = sessions
	}
	return out
}

// forgetEverywhere drops the cached lists, once the host deleted a session.
func (h *Host) forgetEverywhere() {
	h.everywhereMu.Lock()
	clear(h.everywhere)
	h.everywhereMu.Unlock()
}

// startedBackend is a started backend of the provider in any open project.
func (h *Host) startedBackend(provider string) *backendRuntime {
	for _, p := range h.loaded() {
		if b := p.startedBackend(provider); b != nil {
			return b
		}
	}
	return nil
}

// everywhereSessions describes the sessions agents listed for every folder,
// each in its own folder. A folder outside the host's roots cannot open, so
// its sessions are left out.
func (h *Host) everywhereSessions(everywhere map[string][]code.SessionInfo, seen map[string]bool) []types.SessionSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	items := []types.SessionSummary{}
	saved := map[string]map[nativeKey]sessionAlias{}
	for _, provider := range slices.Sorted(maps.Keys(everywhere)) {
		for _, session := range everywhere[provider] {
			if session.Dir == "" {
				continue
			}
			dir := canonicalPath(session.Dir)
			if !h.servesLocked(dir) {
				continue
			}
			if h.listed == nil {
				h.listed = map[string]bool{}
			}
			h.listed[dir] = true
			index, ok := saved[dir]
			if !ok {
				index = h.savedIndexLocked(dir)
				saved[dir] = index
			}
			project, directory := projectInfo(dir), fileuri.FromPath(dir)
			// A folder's open project names it as its windows do.
			if p := h.projects[dir]; p != nil {
				project, directory = p.info(), p.directoryURI()
			}
			if summary, ok := h.listedSummaryLocked(dir, project, directory, provider, session, index, seen); ok {
				items = append(items, summary)
			}
		}
	}
	return items
}
