package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

var (
	errHostClosed      = errors.New("host closed")
	errWorkspaceClosed = errors.New("workspace closed")
	errSessionDeleted  = errors.New("session deleted")
	errUnknownSession  = &ahp.Error{Code: int(types.ErrorCodeSessionNotFound), Message: "unknown session"}
)

// canonicalPath follows the links in the part of a path that exists.
func canonicalPath(path string) string {
	if resolved, err := pathutil.ResolveExistingPrefix(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// relative names path below dir.
func relative(dir, path string) (string, bool) {
	rel, err := filepath.Rel(dir, path)
	return rel, err == nil && (rel == "." || filepath.IsLocal(rel))
}

func projectInfo(dir string) *types.ProjectInfo {
	return &types.ProjectInfo{Uri: fileuri.FromPath(dir), DisplayName: filepath.Base(dir)}
}

// Caller holds h.mu.
func (h *Host) projectsLocked() []*Project {
	projects := slices.Collect(maps.Values(h.projects))
	slices.SortFunc(projects, func(a, b *Project) int { return strings.Compare(a.dir, b.dir) })
	return projects
}

func (h *Host) loaded() []*Project {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.projectsLocked()
}

// serves reports whether projects may be opened in a directory.
func (h *Host) serves(dir string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.servesLocked(dir)
}

// servesLocked is serves for a caller that holds mu.
func (h *Host) servesLocked(dir string) bool {
	return slices.ContainsFunc(h.roots, func(root string) bool {
		_, ok := relative(root, dir)
		return ok
	})
}

// Allow lets projects open in a directory the host's roots do not cover, such
// as a folder the user picked in the desktop app, and lets windows show it.
func (h *Host) Allow(dir string) {
	dir = canonicalPath(dir)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.picked == nil {
		h.picked = map[string]bool{}
	}
	h.picked[dir] = true
	if !h.servesLocked(dir) {
		h.roots = append(h.roots, dir)
	}
}

// known reports whether a browser window may open a project by naming it: one
// that is open, that has saved sessions, or whose folder the user picked. Any
// website can send a browser to a window's address, and opening a project
// starts tools, such as language servers, that may run its code.
func (h *Host) known(dir string) bool {
	dir = canonicalPath(dir)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.projects[dir] != nil || h.picked[dir] || h.listed[dir] {
		return true
	}
	for _, aliases := range []map[string]sessionAlias{h.aliases, h.chatAliases} {
		for _, alias := range aliases {
			if alias.Project == dir {
				return true
			}
		}
	}
	return false
}

// projectAt opens the project of a directory unless it is open already.
func (h *Host) projectAt(dir string) (*Project, error) {
	dir = canonicalPath(dir)
	opened := func() (*Project, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.ctx.Err() != nil {
			return nil, errHostClosed
		}
		p := h.projects[dir]
		if p != nil {
			p.used = time.Now()
		}
		return p, nil
	}
	if p, err := opened(); p != nil || err != nil {
		return p, err
	}
	// Wait for an earlier instance to finish closing before reopening its files.
	// Requests for projects that are already open do not wait here.
	h.projectMu.Lock()
	defer h.projectMu.Unlock()
	if p, err := opened(); p != nil || err != nil {
		return p, err
	}
	if !h.serves(dir) {
		return nil, &ahp.Error{Code: int(types.ErrorCodePermissionDenied), Message: "this host does not serve " + dir}
	}
	p, err := h.open(dir)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.ctx.Err() != nil {
		h.mu.Unlock()
		p.close()
		return nil, errHostClosed
	}
	h.projects[dir] = p
	h.mu.Unlock()
	// Its agents may know their models by now.
	h.publishAgents()
	return p, nil
}

// projectOf finds the open project a path lies in.
func (h *Host) projectOf(path string) *Project {
	path = canonicalPath(path)
	h.mu.Lock()
	defer h.mu.Unlock()
	var found *Project
	for _, p := range h.projectsLocked() {
		if _, ok := relative(p.dir, path); ok && (found == nil || len(p.dir) > len(found.dir)) {
			found = p
		}
	}
	if found != nil {
		found.used = time.Now()
	}
	return found
}

// webProject is the project of the host's web UI, whose clients need not name its directory.
func (h *Host) webProject() *Project {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.web
}

// workingProject opens the project of a session's working directory: the default one if the client names none.
func (h *Host) workingProject(uri *string) (*Project, error) {
	if uri == nil {
		if p := h.webProject(); p != nil {
			return p, nil
		}
		h.mu.Lock()
		root := h.roots[0]
		h.mu.Unlock()
		return h.projectAt(root)
	}
	path, err := ahp.FilePath(*uri)
	if err != nil {
		return nil, err
	}
	return h.projectAt(path)
}

// loadedProject names the open project a directory belongs to.
func (h *Host) loadedProject(uri *string) (*Project, error) {
	if uri == nil {
		if p := h.webProject(); p != nil {
			return p, nil
		}
		return nil, ahp.InvalidParams("a directory is required")
	}
	path, err := ahp.FilePath(*uri)
	if err != nil {
		return nil, err
	}
	if p := h.projectOf(path); p != nil {
		return p, nil
	}
	return nil, ahp.InvalidParams("no session uses this directory")
}

// sessionProject opens the project a session was saved in.
func (h *Host) sessionProject(resource string) (*Project, error) {
	if !ahp.ValidSessionURI(resource) {
		return nil, ahp.InvalidParams("unsupported channel")
	}
	h.mu.Lock()
	alias, ok := h.aliases[resource]
	h.mu.Unlock()
	dir := alias.Project
	if !ok {
		var err error
		if _, _, dir, err = parseCanonicalURI(resource); err != nil {
			return nil, err
		}
	}
	// Canonical URIs from before hosts served several projects are the web UI's.
	if dir == "" {
		if p := h.webProject(); p != nil {
			return p, nil
		}
		return nil, errUnknownSession
	}
	return h.projectAt(dir)
}

// ownsLocked reports whether a terminal, watch or changeset channel is the project's. Caller holds host.mu.
func (p *Project) ownsLocked(channel string) bool {
	return p.terminalChannels[channel] != nil || p.watches[channel] != nil || strings.HasPrefix(channel, p.changesetPrefix())
}

// ownerLocked finds the project a terminal, watch or changeset belongs to.
func (h *Host) ownerLocked(channel string) *Project {
	for _, p := range h.projects {
		if p.ownsLocked(channel) {
			p.used = time.Now()
			return p
		}
	}
	return nil
}

func (h *Host) owner(channel string) *Project {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ownerLocked(channel)
}

// subscribedLocked reports whether a client follows one of the project's channels.
func (h *Host) subscribedLocked(p *Project) bool {
	for c := range h.clients {
		for channel := range c.subscriptions {
			if a, _ := h.channel(channel); a != nil && a.project == p || p.ownsLocked(channel) {
				return true
			}
		}
	}
	return false
}

// closeIdle closes the projects that no client has reached for a while and
// that have no work left. Their sessions stay listed and reopen them.
func (h *Host) closeIdle(idle time.Duration) {
	h.projectMu.Lock()
	defer h.projectMu.Unlock()
	closed := false
	for _, p := range h.loaded() {
		if p.busy() {
			continue
		}
		h.mu.Lock()
		// Any request that could start work reaches the project first, which counts as use.
		if h.ctx.Err() != nil || h.projects[p.dir] != p || p == h.web || p.windowRequests > 0 || time.Since(p.used) < idle || h.subscribedLocked(p) {
			h.mu.Unlock()
			continue
		}
		p.detachLocked()
		h.mu.Unlock()
		p.close()
		closed = true
	}
	if !closed {
		return
	}
	// The root lists the models and terminals of open projects only.
	h.publishAgents()
	h.mu.Lock()
	if h.ctx.Err() == nil {
		h.publishTerminalsLocked()
	}
	h.mu.Unlock()
}

// resource serves the files of a project.
func (h *Host) resource(req ahp.Request) (any, error) {
	var p resourceParams
	if json.Unmarshal(req.Params, &p) != nil || p.Channel != types.RootResourceURI {
		return nil, ahp.InvalidParams("root channel required")
	}
	uri := cmp.Or(p.URI, p.Source)
	if strings.HasPrefix(uri, "ahp-content:") {
		projects := h.loaded()
		i := slices.IndexFunc(projects, func(project *Project) bool { return project.retains(uri) })
		if i < 0 {
			return nil, &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "changeset content is no longer retained; refresh the changeset"}
		}
		return projects[i].resource(req)
	}
	path, err := ahp.FilePath(uri)
	if err != nil {
		return nil, err
	}
	if project := h.projectOf(path); project != nil {
		return project.resource(req)
	}
	return h.browse(req.Method, p, path)
}

// browse reads outside any project, so that a client can choose a directory.
func (h *Host) browse(method string, p resourceParams, path string) (any, error) {
	switch method {
	case "resourceList", "resourceResolve", "resourceRead":
	case "resourceRequest":
		if p.Write {
			return nil, ahp.InvalidParams("start a session in this directory to change its files")
		}
	default:
		return nil, ahp.InvalidParams("start a session in this directory to change its files")
	}
	if p.Encoding != "" && p.Encoding != types.ContentEncodingUtf8 && p.Encoding != types.ContentEncodingBase64 {
		return nil, ahp.InvalidParams("unsupported encoding")
	}
	if !h.serves(canonicalPath(path)) {
		return nil, &ahp.Error{Code: int(types.ErrorCodePermissionDenied), Message: "this host does not serve " + path}
	}
	// A directory is its own tree; a file is read from its parent's.
	dir, rel := path, "."
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		dir, rel = filepath.Dir(path), filepath.Base(path)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return resourceFiles{root: root, dir: dir}.read(method, p, rel)
}
