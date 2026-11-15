package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

var errHostClosed = errors.New("host closed")

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

func ahpFilePath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Host != "" && u.Host != "localhost" || u.RawQuery != "" || u.Fragment != "" {
		return "", ahpInvalid("local file URI required")
	}
	path, _ := fileuri.Path(uri)
	if !filepath.IsAbs(path) {
		return "", ahpInvalid("absolute file URI required")
	}
	return path, nil
}

func projectInfo(dir string) *t.ProjectInfo {
	return &t.ProjectInfo{Uri: fileuri.FromPath(dir), DisplayName: filepath.Base(dir)}
}

// Caller holds h.mu.
func (h *ahpHost) projectsLocked() []*ahpProject {
	if h.pinned != nil {
		return []*ahpProject{h.pinned}
	}
	projects := slices.Collect(maps.Values(h.projects))
	slices.SortFunc(projects, func(a, b *ahpProject) int { return strings.Compare(a.dir, b.dir) })
	return projects
}

func (h *ahpHost) loaded() []*ahpProject {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.projectsLocked()
}

// serves reports whether projects may be opened in a directory.
func (h *ahpHost) serves(dir string) bool {
	return slices.ContainsFunc(h.roots, func(root string) bool {
		_, ok := relative(root, dir)
		return ok
	})
}

// projectAt opens the project of a directory unless it is open already.
func (h *ahpHost) projectAt(dir string) (*ahpProject, error) {
	dir = canonicalPath(dir)
	opened := func() (*ahpProject, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed {
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
		return nil, &ahpRPCError{Code: int(t.ErrorCodePermissionDenied), Message: "this host does not serve " + dir}
	}
	s, err := h.open(dir)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		s.Close()
		return nil, errHostClosed
	}
	h.projects[dir] = s.ahp
	h.mu.Unlock()
	// Its agents may know their models by now.
	h.publishAgents()
	return s.ahp, nil
}

// projectOf finds the open project a path lies in.
func (h *ahpHost) projectOf(path string) *ahpProject {
	if h.pinned != nil {
		return h.pinned
	}
	path = canonicalPath(path)
	h.mu.Lock()
	defer h.mu.Unlock()
	var found *ahpProject
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

// workingProject opens the project of a session's working directory: the default one if the client names none.
func (h *ahpHost) workingProject(uri *string) (*ahpProject, error) {
	if h.pinned != nil {
		return h.pinned, nil
	}
	if uri == nil {
		return h.projectAt(h.roots[0])
	}
	path, err := ahpFilePath(*uri)
	if err != nil {
		return nil, err
	}
	return h.projectAt(path)
}

// loadedProject names the open project a directory belongs to.
func (h *ahpHost) loadedProject(uri *string) (*ahpProject, error) {
	if h.pinned != nil {
		return h.pinned, nil
	}
	if uri == nil {
		return nil, ahpInvalid("a directory is required")
	}
	path, err := ahpFilePath(*uri)
	if err != nil {
		return nil, err
	}
	if p := h.projectOf(path); p != nil {
		return p, nil
	}
	return nil, ahpInvalid("no session uses this directory")
}

// sessionProject opens the project a session was saved in.
func (h *ahpHost) sessionProject(resource string) (*ahpProject, error) {
	if h.pinned != nil {
		return h.pinned, nil
	}
	if !validAHPSessionURI(resource) {
		return nil, ahpInvalid("unsupported channel")
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
	if dir == "" {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown session"}
	}
	return h.projectAt(dir)
}

// ownerLocked finds the project a terminal, watch or changeset belongs to.
func (h *ahpHost) ownerLocked(channel string) *ahpProject {
	for _, p := range h.projectsLocked() {
		if p.terminals[channel] != nil || p.watches[channel] != nil || strings.HasPrefix(channel, p.changesetPrefix()) {
			p.used = time.Now()
			return p
		}
	}
	return nil
}

func (h *ahpHost) owner(channel string) *ahpProject {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ownerLocked(channel)
}

// subscribedLocked reports whether a client follows one of the project's channels.
func (h *ahpHost) subscribedLocked(p *ahpProject) bool {
	for c := range h.clients {
		for channel := range c.subscriptions {
			if a, _ := h.channel(channel); a != nil && a.project == p {
				return true
			}
			if p.terminals[channel] != nil || p.watches[channel] != nil || p.changesets[channel] != nil {
				return true
			}
		}
	}
	return false
}

// closeIdle closes the projects that no client has reached for a while and
// that have no work left. Their sessions stay listed and reopen them.
func (h *ahpHost) closeIdle(idle time.Duration) {
	h.projectMu.Lock()
	defer h.projectMu.Unlock()
	for _, p := range h.loaded() {
		if p.server.busy() {
			continue
		}
		h.mu.Lock()
		// Any request that could start work reaches the project first, which counts as use.
		if h.closed || h.projects[p.dir] != p || time.Since(p.used) < idle || h.subscribedLocked(p) {
			h.mu.Unlock()
			continue
		}
		p.detachLocked()
		h.mu.Unlock()
		p.server.Close()
	}
}

// resource serves the files of a project.
func (h *ahpHost) resource(req ahpRequest) (any, error) {
	if h.pinned != nil {
		return h.pinned.resource(req)
	}
	var p ahpResourceParams
	if json.Unmarshal(req.Params, &p) != nil || p.Channel != t.RootResourceURI {
		return nil, ahpInvalid("root channel required")
	}
	uri := cmp.Or(p.URI, p.Source)
	if strings.HasPrefix(uri, "ahp-content:") {
		projects := h.loaded()
		i := slices.IndexFunc(projects, func(project *ahpProject) bool { return project.retains(uri) })
		if i < 0 {
			return nil, &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "changeset content is no longer retained; refresh the changeset"}
		}
		return projects[i].resource(req)
	}
	path, err := ahpFilePath(uri)
	if err != nil {
		return nil, err
	}
	if project := h.projectOf(path); project != nil {
		return project.resource(req)
	}
	return h.browse(req.Method, p, path)
}

// browse reads outside any project, so that a client can choose a directory.
func (h *ahpHost) browse(method string, p ahpResourceParams, path string) (any, error) {
	switch method {
	case "resourceList", "resourceResolve", "resourceRead":
	case "resourceRequest":
		if p.Write {
			return nil, ahpInvalid("start a session in this directory to change its files")
		}
	default:
		return nil, ahpInvalid("start a session in this directory to change its files")
	}
	if p.Encoding != "" && p.Encoding != t.ContentEncodingUtf8 && p.Encoding != t.ContentEncodingBase64 {
		return nil, ahpInvalid("unsupported encoding")
	}
	if !h.serves(canonicalPath(path)) {
		return nil, &ahpRPCError{Code: int(t.ErrorCodePermissionDenied), Message: "this host does not serve " + path}
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
	return ahpFiles{root: root, dir: dir}.read(method, p, rel)
}
