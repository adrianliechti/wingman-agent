package server

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// sessionAlias maps an AHP URI to the native session behind it. A session's
// alias names its default chat; a forked chat's alias also names the chat.
type sessionAlias struct {
	Resource      string                `json:"resource"`
	Project       string                `json:"project,omitempty"`     // directory
	StorageRoot   string                `json:"storageRoot,omitempty"` // repository owning a builtin journal, including after a worktree is removed
	Chat          string                `json:"chat,omitempty"`
	Title         string                `json:"title,omitempty"`
	Backend       string                `json:"backend"`
	Session       string                `json:"session"`
	Fork          string                `json:"fork,omitempty"` // source chat
	Turn          string                `json:"turn,omitempty"` // source turn
	Created       time.Time             `json:"created,omitzero"`
	Modified      time.Time             `json:"modified,omitzero"` // the session's last change
	Status        types.SessionStatus   `json:"status,omitempty"`  // the session's archived flag
	Unread        bool                  `json:"unread,omitempty"`  // activity no client has seen yet
	Changes       *types.ChangesSummary `json:"changes,omitempty"` // the files its default chat changed
	CreationError string                `json:"creationError,omitempty"`
}

// flags are a saved session's read and archived flags. A session is read
// unless it was saved with activity no client had seen, so sessions saved
// before the host tracked reading do not all show as new.
func (a sessionAlias) flags() types.SessionStatus {
	flags := a.Status & types.SessionStatusIsArchived
	if !a.Unread {
		flags |= types.SessionStatusIsRead
	}
	return flags
}

// aliasStore serializes the host's persistence of aliases, of open and closed
// projects alike, without holding the publication lock. Pending changes are
// protected by host.mu and stay pending until written.
type aliasStore struct {
	mu      sync.Mutex
	pending map[string]*aliasWrite
}

type aliasWrite struct {
	alias  sessionAlias
	remove bool
}

func (a sessionAlias) key() string {
	if a.Chat != "" {
		return a.Chat
	}
	return a.Resource
}

// Caller holds h.mu. A newer change supersedes an older write or deletion.
func (h *Host) stageAliasLocked(alias sessionAlias, remove bool) {
	if h.aliasWrites.pending == nil {
		h.aliasWrites.pending = map[string]*aliasWrite{}
	}
	h.aliasWrites.pending[alias.key()] = &aliasWrite{alias: alias, remove: remove}
}

func (h *Host) flushAliases() error {
	h.mu.Lock()
	clean := len(h.aliasWrites.pending) == 0
	h.mu.Unlock()
	if clean {
		return nil
	}
	h.aliasWrites.mu.Lock()
	defer h.aliasWrites.mu.Unlock()
	h.mu.Lock()
	keys := slices.Collect(maps.Keys(h.aliasWrites.pending))
	h.mu.Unlock()
	var errs []error
	for _, key := range keys {
		if err := h.flushAlias(key); err != nil {
			errs = append(errs, fmt.Errorf("persist %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// Caller holds aliasWrites.mu. Recheck after I/O so an older write cannot
// clear a newer pending change or resurrect a deleted alias.
func (h *Host) flushAlias(key string) error {
	for {
		h.mu.Lock()
		change := h.aliasWrites.pending[key]
		h.mu.Unlock()
		if change == nil {
			return nil
		}
		var err error
		if change.remove {
			err = removeAlias(change.alias)
		} else {
			err = writeAlias(change.alias)
		}
		h.mu.Lock()
		current := h.aliasWrites.pending[key] == change
		if current && err == nil {
			delete(h.aliasWrites.pending, key)
		}
		h.mu.Unlock()
		if current {
			return err
		}
	}
}

// Published metadata is retried on the next update and during project teardown.
func (h *Host) saveAliases() {
	if err := h.flushAliases(); err != nil {
		fmt.Fprintf(os.Stderr, "session metadata warning: %v\n", err)
	}
}

func readAliases(dir string) []sessionAlias {
	var aliases []sessionAlias
	files, _ := os.ReadDir(dir)
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, file.Name()))
		var alias sessionAlias
		if err == nil && json.Unmarshal(data, &alias) == nil && ahp.ValidSessionURI(alias.Resource) && alias.Backend != "" && alias.Session != "" {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

// rooted names the repository that keeps a builtin session's journal while
// the session's folder still tells. It reads the disk, so callers do it
// before they take the host's lock.
func (a sessionAlias) rooted() sessionAlias {
	if a.Backend == code.BuiltinAgentName && a.StorageRoot == "" && a.Project != "" {
		if info, err := os.Stat(a.Project); err == nil && info.IsDir() {
			a.StorageRoot = code.SessionStorageRoot(a.Project)
		}
	}
	return a
}

// Caller holds h.mu.
func (h *Host) rememberAliasLocked(alias sessionAlias) {
	if alias.Chat != "" {
		h.chatAliases[alias.Chat] = alias
	} else {
		h.aliases[alias.Resource] = alias
	}
}

// aliasDirectory is where the host keeps the aliases of a project's sessions.
func aliasDirectory(project string) (string, error) {
	return layout.WingmanPath("ahp", workspaceID(project))
}

func (p *Project) aliasDirectory() (string, error) { return aliasDirectory(p.dir) }

// writeAlias saves what the host keeps of a session with its project's, which
// need not be open.
func writeAlias(alias sessionAlias) error {
	dir, err := aliasDirectory(alias.Project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(alias)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), aliasPath(dir, alias))
}

func aliasPath(dir string, alias sessionAlias) string {
	sum := sha256.Sum256([]byte(alias.key()))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

// Caller holds h.mu and flushes persistence after releasing it.
func (h *Host) forgetChatAliasLocked(chat string) {
	alias, ok := h.chatAliases[chat]
	if !ok {
		return
	}
	delete(h.chatAliases, chat)
	h.stageAliasLocked(alias, true)
}

// Caller holds h.mu and flushes persistence after releasing it.
func (h *Host) forgetAliasLocked(resource string) {
	alias, ok := h.aliases[resource]
	if !ok {
		return
	}
	delete(h.aliases, resource)
	h.stageAliasLocked(alias, true)
}

// forgetSessionLocked drops a session that is gone, with its forks, from what
// the host keeps and tells the clients that list sessions. Caller holds h.mu
// and flushes persistence after releasing it.
func (h *Host) forgetSessionLocked(resource string) {
	for _, fork := range h.forksLocked(resource) {
		h.forgetChatAliasLocked(fork.Chat)
	}
	h.forgetAliasLocked(resource)
	h.forgetEverywhere()
	h.notifyRoot("root/sessionRemoved", types.SessionRemovedParams{Channel: types.RootResourceURI, Session: resource})
}

// forgetSession drops a deleted session that no client has open. Its project
// may open meanwhile and read its aliases from disk, so that waits until the
// change is written.
func (h *Host) forgetSession(resource string) error {
	h.projectMu.Lock()
	defer h.projectMu.Unlock()
	h.mu.Lock()
	h.forgetSessionLocked(resource)
	h.mu.Unlock()
	return h.flushAliases()
}

func removeAlias(alias sessionAlias) error {
	dir, err := aliasDirectory(alias.Project)
	if err != nil {
		return err
	}
	err = os.Remove(aliasPath(dir, alias))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// nativeKey names a native session, whose id is unique only within its project and agent.
type nativeKey struct{ project, backend, session string }

func (a sessionAlias) native() nativeKey {
	return nativeKey{cmp.Or(a.StorageRoot, a.Project), a.Backend, a.Session}
}

func nativeSessionKey(dir, backend, id string) nativeKey {
	if backend == code.BuiltinAgentName {
		dir = code.SessionStorageRoot(dir)
	}
	return nativeKey{dir, backend, id}
}

// forksLocked returns the saved chats in the same order for loading and listing.
// Caller holds h.mu.
func (h *Host) forksLocked(resource string) []sessionAlias {
	var forks []sessionAlias
	for _, alias := range h.chatAliases {
		if alias.Resource == resource {
			forks = append(forks, alias)
		}
	}
	slices.SortFunc(forks, func(a, b sessionAlias) int { return a.Created.Compare(b.Created) })
	return forks
}

// savedIndexLocked finds aliases by native session. Builtin journals may be
// shared by several worktrees. A fork's chat alias wins. Caller holds h.mu.
func (h *Host) savedIndexLocked() map[nativeKey]sessionAlias {
	index := map[nativeKey]sessionAlias{}
	for _, aliases := range []map[string]sessionAlias{h.aliases, h.chatAliases} {
		for _, alias := range aliases {
			index[alias.native()] = alias
		}
	}
	return index
}

// discard undoes a session or chat whose creation failed: it leaves the host,
// and its native session is deleted.
func (p *Project) discard(c *sessionController, alias sessionAlias) {
	p.host.mu.Lock()
	if alias.Chat != "" {
		p.host.forgetChatAliasLocked(alias.Chat)
	} else {
		p.host.forgetAliasLocked(alias.Resource)
	}
	p.host.mu.Unlock()
	p.host.saveAliases()
	_ = c.command(c.backend.Project.ctx, Command{ID: "ahp-discard:" + c.ref.SessionID, Type: "delete"})
}

// discardSaved discards a session whose creation was cut off, in its own project and agent.
func (h *Host) discardSaved(alias sessionAlias) error {
	p, err := h.projectAt(alias.Project)
	if err != nil {
		return err
	}
	b, err := p.backend(alias.Backend)
	if err != nil {
		return err
	}
	if !p.beginOperation() {
		return errWorkspaceClosed
	}
	defer p.operations.Done()
	p.discard(b.session(alias.Session), alias)
	return nil
}

// storeAlias saves a session's or chat's alias and makes it known to the host.
func (p *Project) storeAlias(alias sessionAlias) error {
	alias.Project = p.dir
	alias = alias.rooted()
	p.host.mu.Lock()
	p.host.rememberAliasLocked(alias)
	p.host.stageAliasLocked(alias, false)
	p.host.mu.Unlock()
	p.host.aliasWrites.mu.Lock()
	defer p.host.aliasWrites.mu.Unlock()
	return p.host.flushAlias(alias.key())
}

func (h *Host) loadSavedSessions() {
	dir, err := layout.WingmanPath("ahp")
	if err != nil {
		return
	}
	projects, _ := os.ReadDir(dir)
	var saved []sessionAlias
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		for _, alias := range readAliases(filepath.Join(dir, project.Name())) {
			// Sessions saved before hosts served several projects name none.
			if alias.Project != "" && h.serves(alias.Project) {
				saved = append(saved, alias.rooted())
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, alias := range saved {
		h.rememberAliasLocked(alias)
	}
}
