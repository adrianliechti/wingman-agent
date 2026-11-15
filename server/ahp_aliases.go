package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/layout"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// ahpAlias maps an AHP URI to the native session behind it. A session's
// alias names its default chat; a forked chat's alias also names the chat.
type ahpAlias struct {
	Resource      string          `json:"resource"`
	Project       string          `json:"project,omitempty"` // directory
	Chat          string          `json:"chat,omitempty"`
	Title         string          `json:"title,omitempty"`
	Backend       string          `json:"backend"`
	Session       string          `json:"session"`
	Fork          string          `json:"fork,omitempty"` // source chat
	Turn          string          `json:"turn,omitempty"` // source turn
	Created       time.Time       `json:"created,omitzero"`
	Modified      time.Time       `json:"modified,omitzero"` // the session's last change
	Status        t.SessionStatus `json:"status,omitempty"`  // the session's own read and archived flags
	CreationError string          `json:"creationError,omitempty"`
}

func readAliases(dir string) []ahpAlias {
	var aliases []ahpAlias
	files, _ := os.ReadDir(dir)
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, file.Name()))
		var alias ahpAlias
		if err == nil && json.Unmarshal(data, &alias) == nil && validAHPSessionURI(alias.Resource) && alias.Backend != "" && alias.Session != "" {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

// Caller holds h.mu.
func (h *ahpHost) rememberAliasLocked(alias ahpAlias) {
	if alias.Chat != "" {
		h.chatAliases[alias.Chat] = alias
	} else {
		h.aliases[alias.Resource] = alias
	}
}

func (p *ahpProject) aliasDirectory() (string, error) {
	return layout.WingmanPath("ahp", p.server.scope.WorkspaceID)
}

func (p *ahpProject) saveAlias(alias ahpAlias) error {
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
	return os.Rename(file.Name(), p.aliasPath(dir, alias))
}

func (h *ahpHost) aliasPath(dir string, alias ahpAlias) string {
	key := alias.Resource
	if alias.Chat != "" {
		key = alias.Chat
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

// Caller holds p.mu.
func (p *ahpProject) forgetChatAlias(chat string) {
	alias, ok := p.chatAliases[chat]
	if !ok {
		return
	}
	delete(p.chatAliases, chat)
	p.removeAlias(alias)
}

// Caller holds p.mu.
func (p *ahpProject) forgetAlias(resource string) {
	alias, ok := p.aliases[resource]
	if !ok {
		return
	}
	delete(p.aliases, resource)
	p.removeAlias(alias)
}

func (p *ahpProject) removeAlias(alias ahpAlias) {
	if dir, err := p.aliasDirectory(); err == nil {
		_ = os.Remove(p.aliasPath(dir, alias))
	}
}

// nativeKey names a native session, whose id is unique only within its project and agent.
type nativeKey struct{ project, backend, session string }

func (a ahpAlias) native() nativeKey { return nativeKey{a.Project, a.Backend, a.Session} }

// forksLocked returns the saved chats in the same order for loading and listing.
// Caller holds h.mu.
func (h *ahpHost) forksLocked(resource string) []ahpAlias {
	var forks []ahpAlias
	for _, alias := range h.chatAliases {
		if alias.Resource == resource {
			forks = append(forks, alias)
		}
	}
	slices.SortFunc(forks, func(a, b ahpAlias) int { return a.Created.Compare(b.Created) })
	return forks
}

// savedLocked finds the alias of a native session: its forked chat's, or its session's.
func (h *ahpHost) savedLocked(key nativeKey) (ahpAlias, bool) {
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
	return ahpAlias{}, false
}

// discard undoes a session or chat whose creation failed: it leaves the host,
// and its native session is deleted.
func (p *ahpProject) discard(c *sessionController, alias ahpAlias) {
	p.mu.Lock()
	if alias.Chat != "" {
		p.forgetChatAlias(alias.Chat)
	} else {
		p.forgetAlias(alias.Resource)
	}
	p.mu.Unlock()
	_ = c.command(c.backend.ctx, Command{ID: "ahp-discard:" + c.ref.SessionID, Epoch: c.epoch, Type: "delete"})
}

// storeAlias saves a session's or chat's alias and makes it known to the host.
func (p *ahpProject) storeAlias(alias ahpAlias) error {
	alias.Project = p.dir
	p.aliasMu.Lock()
	defer p.aliasMu.Unlock()
	if err := p.saveAlias(alias); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rememberAliasLocked(alias)
	return nil
}

func (h *ahpHost) loadSavedSessions() {
	dir, err := layout.WingmanPath("ahp")
	if err != nil {
		return
	}
	projects, _ := os.ReadDir(dir)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		for _, alias := range readAliases(filepath.Join(dir, project.Name())) {
			// Sessions saved before hosts served several projects name none.
			if alias.Project != "" && h.serves(alias.Project) {
				h.rememberAliasLocked(alias)
			}
		}
	}
}
