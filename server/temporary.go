package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/filelock"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// A temporary chat works in a folder of its own below temporaryRoot. The host
// that opens it holds its lock and, once no window has shown it for
// temporaryGrace, deletes its sessions, the folder and Wingman's state of it.

const (
	temporaryGrace = time.Minute
	temporaryName  = "Temporary Chat"
)

var temporaryRoots sync.Map // os.TempDir() → its folder of temporary chats, links resolved

func temporaryRoot() string {
	tmp := os.TempDir()
	if root, ok := temporaryRoots.Load(tmp); ok {
		return root.(string)
	}
	root := canonicalPath(filepath.Join(tmp, "wingman-chats"))
	temporaryRoots.Store(tmp, root)
	return root
}

// isTemporary reports whether a directory, with its links resolved, is a temporary chat's.
func isTemporary(dir string) bool {
	return filepath.Dir(dir) == temporaryRoot()
}

// NewTemporaryFolder makes the folder of a temporary chat. The host that
// opens it removes it again.
func NewTemporaryFolder() (string, error) {
	root := temporaryRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	// Other users may share the temporary directory; a root they could write to is not ours.
	if info, err := os.Stat(root); err != nil || !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("%s is not a private directory", root)
	}
	return os.MkdirTemp(root, "chat-")
}

// claimTemporary locks a temporary chat's folder for this process and returns
// what removes it. It fails while another Wingman has the chat open.
func claimTemporary(dir string) (func(), error) {
	file, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	unlock, err := filelock.Lock(file, false)
	if err != nil {
		file.Close()
		return nil, errors.New("this temporary chat is open in another Wingman")
	}
	return func() {
		state := code.ProjectStateDir(dir)
		// A folder inside a repository keeps its state with the repository's.
		shared := code.SessionStorageRoot(dir) != dir
		_ = os.RemoveAll(dir)
		if !shared {
			_ = os.RemoveAll(state)
		}
		if aliases, err := aliasDirectory(dir); err == nil {
			_ = os.RemoveAll(aliases)
		}
		unlock()
		file.Close()
		_ = os.Remove(dir + ".lock")
	}, nil
}

// sweepTemporary removes the temporary chats no Wingman has open, which a
// host that stopped unexpectedly leaves. One made a moment ago may not be
// open yet.
func sweepTemporary() {
	root := temporaryRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	names := map[string]bool{}
	for _, entry := range entries {
		if name := strings.TrimSuffix(entry.Name(), ".lock"); strings.HasPrefix(name, "chat-") {
			names[name] = true
		}
	}
	for name := range names {
		dir := filepath.Join(root, name)
		if info, err := os.Stat(dir); err == nil && time.Since(info.ModTime()) < temporaryGrace {
			continue
		}
		if remove, err := claimTemporary(dir); err == nil {
			remove()
		}
	}
}

// aliasesLocked lists the saved sessions and chats of a project. Caller holds h.mu.
func (h *Host) aliasesLocked(dir string) []sessionAlias {
	var aliases []sessionAlias
	for _, alias := range h.chatAliases {
		if alias.Project == dir {
			aliases = append(aliases, alias)
		}
	}
	for _, alias := range h.aliases {
		if alias.Project == dir {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

// deleteAgentSessions deletes what other agents keep of a temporary project's
// sessions while they still run; Wingman's own go with the project's state.
func (p *Project) deleteAgentSessions() {
	p.host.mu.Lock()
	aliases := p.host.aliasesLocked(p.dir)
	p.host.mu.Unlock()
	if !p.beginOperation() {
		return
	}
	defer p.operations.Done()
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()
	for _, alias := range aliases {
		if b := p.startedBackend(alias.Backend); alias.Backend != code.BuiltinAgentName && b != nil && supportsDelete(b.agent) {
			_ = b.agent.DeleteSession(ctx, alias.Session)
		}
	}
}

// forgetTemporary drops a closed temporary project's sessions from the lists.
func (h *Host) forgetTemporary(dir string) {
	h.mu.Lock()
	for _, alias := range h.aliasesLocked(dir) {
		if alias.Chat != "" {
			h.forgetChatAliasLocked(alias.Chat)
		} else {
			h.forgetSessionLocked(alias.Resource)
		}
	}
	h.mu.Unlock()
	h.saveAliases()
}
