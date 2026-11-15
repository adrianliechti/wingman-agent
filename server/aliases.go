package server

import (
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

	"github.com/adrianliechti/wingman-agent/pkg/layout"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// sessionAlias maps an AHP URI to the native session behind it. A session's
// alias names its default chat; a forked chat's alias also names the chat.
type sessionAlias struct {
	Resource      string              `json:"resource"`
	Project       string              `json:"project,omitempty"` // directory
	Chat          string              `json:"chat,omitempty"`
	Title         string              `json:"title,omitempty"`
	Backend       string              `json:"backend"`
	Session       string              `json:"session"`
	Fork          string              `json:"fork,omitempty"` // source chat
	Turn          string              `json:"turn,omitempty"` // source turn
	Created       time.Time           `json:"created,omitzero"`
	Modified      time.Time           `json:"modified,omitzero"` // the session's last change
	Status        types.SessionStatus `json:"status,omitempty"`  // the session's own read and archived flags
	CreationError string              `json:"creationError,omitempty"`
}

// aliasStore serializes persistence without holding the publication lock.
// Pending changes are protected by host.mu and stay pending until written.
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

// Caller holds host.mu. A newer change supersedes an older write or deletion.
func (p *Project) stageAliasLocked(alias sessionAlias, remove bool) {
	if p.aliases.pending == nil {
		p.aliases.pending = map[string]*aliasWrite{}
	}
	p.aliases.pending[alias.key()] = &aliasWrite{alias: alias, remove: remove}
}

func (p *Project) flushAliases() error {
	p.host.mu.Lock()
	clean := len(p.aliases.pending) == 0
	p.host.mu.Unlock()
	if clean {
		return nil
	}
	p.aliases.mu.Lock()
	defer p.aliases.mu.Unlock()
	p.host.mu.Lock()
	keys := slices.Collect(maps.Keys(p.aliases.pending))
	p.host.mu.Unlock()
	var errs []error
	for _, key := range keys {
		if err := p.flushAlias(key); err != nil {
			errs = append(errs, fmt.Errorf("persist %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// Caller holds aliases.mu. Recheck after I/O so an older write cannot clear
// a newer pending change or resurrect a deleted alias.
func (p *Project) flushAlias(key string) error {
	for {
		p.host.mu.Lock()
		change := p.aliases.pending[key]
		p.host.mu.Unlock()
		if change == nil {
			return nil
		}
		var err error
		if change.remove {
			err = p.removeAlias(change.alias)
		} else {
			err = p.saveAlias(change.alias)
		}
		p.host.mu.Lock()
		current := p.aliases.pending[key] == change
		if current && err == nil {
			delete(p.aliases.pending, key)
		}
		p.host.mu.Unlock()
		if current {
			return err
		}
	}
}

// Published metadata is retried on the next update and during project teardown.
func (p *Project) saveAliases() {
	if err := p.flushAliases(); err != nil {
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

// Caller holds h.mu.
func (h *Host) rememberAliasLocked(alias sessionAlias) {
	if alias.Chat != "" {
		h.chatAliases[alias.Chat] = alias
	} else {
		h.aliases[alias.Resource] = alias
	}
}

func (p *Project) aliasDirectory() (string, error) {
	return layout.WingmanPath("ahp", p.scope.WorkspaceID)
}

func (p *Project) saveAlias(alias sessionAlias) error {
	dir, err := p.aliasDirectory()
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

// Caller holds host.mu and flushes persistence after releasing it.
func (p *Project) forgetChatAlias(chat string) {
	alias, ok := p.host.chatAliases[chat]
	if !ok {
		return
	}
	delete(p.host.chatAliases, chat)
	p.stageAliasLocked(alias, true)
}

// Caller holds host.mu and flushes persistence after releasing it.
func (p *Project) forgetAlias(resource string) {
	alias, ok := p.host.aliases[resource]
	if !ok {
		return
	}
	delete(p.host.aliases, resource)
	p.stageAliasLocked(alias, true)
}

func (p *Project) removeAlias(alias sessionAlias) error {
	dir, err := p.aliasDirectory()
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

func (a sessionAlias) native() nativeKey { return nativeKey{a.Project, a.Backend, a.Session} }

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

// savedLocked finds the alias of a native session: its forked chat's, or its session's.
func (h *Host) savedLocked(key nativeKey) (sessionAlias, bool) {
	for _, alias := range h.chatAliases {
		if alias.native() == key {
			return alias, true
		}
	}
	for _, alias := range h.aliases {
		if alias.native() == key {
			return alias, true
		}
	}
	return sessionAlias{}, false
}

// discard undoes a session or chat whose creation failed: it leaves the host,
// and its native session is deleted.
func (p *Project) discard(c *sessionController, alias sessionAlias) {
	p.host.mu.Lock()
	if alias.Chat != "" {
		p.forgetChatAlias(alias.Chat)
	} else {
		p.forgetAlias(alias.Resource)
	}
	p.host.mu.Unlock()
	p.saveAliases()
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
		return errors.New("workspace closed")
	}
	defer p.operations.Done()
	p.discard(b.session(alias.Session), alias)
	return nil
}

// storeAlias saves a session's or chat's alias and makes it known to the host.
func (p *Project) storeAlias(alias sessionAlias) error {
	alias.Project = p.dir
	p.host.mu.Lock()
	p.host.rememberAliasLocked(alias)
	p.stageAliasLocked(alias, false)
	p.host.mu.Unlock()
	p.aliases.mu.Lock()
	defer p.aliases.mu.Unlock()
	return p.flushAlias(alias.key())
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
				saved = append(saved, alias)
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, alias := range saved {
		h.rememberAliasLocked(alias)
	}
}
