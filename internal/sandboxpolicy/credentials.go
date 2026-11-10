package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/layout"
)

// CredentialHomePaths are the credential stores beneath the user's home
// directory. Sandboxed commands cannot read them, and file tools and prompt
// loaders refuse paths that resolve into them.
var CredentialHomePaths = []string{".ssh", ".aws", ".gnupg", ".azure", ".kube", ".netrc", ".git-credentials", ".docker/config.json", ".config/gh", ".config/gcloud"}

// CredentialStateFiles are the credential files in Wingman's own state directory.
var CredentialStateFiles = []string{"mcp-credentials.json"}

// CredentialPaths returns the existing credential stores with links resolved.
func CredentialPaths() []string {
	var paths []string
	for _, store := range credentialStores() {
		paths = append(paths, store.path)
	}
	return paths
}

type credentialStore struct {
	name string // As configured, before links are resolved.
	path string
	info os.FileInfo
}

func credentialStores() []credentialStore {
	var names []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, rel := range CredentialHomePaths {
			names = append(names, filepath.Join(home, filepath.FromSlash(rel)))
		}
	}
	for _, name := range CredentialStateFiles {
		if path, err := layout.WingmanPath(name); err == nil {
			names = append(names, path)
		}
	}
	var stores []credentialStore
	for _, name := range names {
		path := Canonical(name)
		if _, err := os.Lstat(path); err != nil || slices.ContainsFunc(stores, func(store credentialStore) bool { return store.path == path }) {
			continue
		}
		info, _ := os.Stat(path)
		stores = append(stores, credentialStore{name: name, path: path, info: info})
	}
	return stores
}

// CredentialError reports a path that resolves into a credential store.
type CredentialError struct {
	Path  string
	Store string
}

func (e *CredentialError) Error() string {
	return fmt.Sprintf("path %q resolves into protected credential location %q", e.Path, e.Store)
}

// matches compares by spelling and by file identity. Identity also covers
// spellings that only the filesystem considers equal, and hard links to a
// credential file.
func (s credentialStore) matches(path string, info os.FileInfo) bool {
	return samePath(s.path, path) || (info != nil && s.info != nil && os.SameFile(s.info, info))
}

// CredentialGuard checks paths against one snapshot of the credential stores.
// Take a new snapshot per operation: stores and links can appear at any time.
type CredentialGuard struct {
	stores []credentialStore
	// workspace is set only when the workspace lies inside a store. Its own
	// files stay accessible; the rest of that store does not.
	workspace *credentialStore
}

// NewCredentialGuard snapshots the stores hidden from workspace. A workspace
// the user opened inside a store is not locked out of its own files. That
// exemption follows the workspace's spelling, not its resolved location: a
// link that redirects a workspace into a store earns nothing.
func NewCredentialGuard(workspace string) CredentialGuard {
	guard := CredentialGuard{stores: credentialStores()}
	if workspace == "" {
		return guard
	}
	spelled, err := filepath.Abs(workspace)
	if err != nil {
		return guard
	}
	for i, store := range guard.stores {
		if !spelledWithin(store.name, spelled) && !spelledWithin(store.path, spelled) {
			continue
		}
		own := credentialStore{path: Canonical(spelled)}
		own.info, _ = os.Stat(own.path)
		if store.matches(own.path, own.info) {
			// A store that is the workspace itself hides nothing from it.
			guard.stores = slices.Delete(guard.stores, i, i+1)
		} else {
			guard.workspace = &own
		}
		break
	}
	return guard
}

// Check returns a *CredentialError when path, after resolving links, lies in a
// credential store. The nearest enclosing boundary decides, so a workspace
// inside a store keeps its files while a store inside a workspace stays hidden.
func (g CredentialGuard) Check(path string) error {
	if len(g.stores) == 0 {
		return nil
	}
	for current := Canonical(path); ; {
		info, _ := os.Stat(current)
		if g.workspace != nil && g.workspace.matches(current, info) {
			return nil
		}
		for _, store := range g.stores {
			if store.matches(current, info) {
				return &CredentialError{Path: path, Store: store.path}
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// Named reports whether a directory entry called name could be a credential
// store. Walks use it to skip the path work for every other entry.
func (g CredentialGuard) Named(name string) bool {
	return slices.ContainsFunc(g.stores, func(store credentialStore) bool {
		return strings.EqualFold(name, filepath.Base(store.path))
	})
}

// Hides reports whether the entry at path is itself a credential store. It is
// the per-entry test for walks that start in a checked, resolved directory and
// do not follow links; info is consulted only when the name matches a store.
func (g CredentialGuard) Hides(path string, info func() (os.FileInfo, error)) bool {
	name := filepath.Base(path)
	for _, store := range g.stores {
		if !strings.EqualFold(name, filepath.Base(store.path)) {
			continue
		}
		if samePath(store.path, path) {
			return true
		}
		if info == nil || store.info == nil {
			continue
		}
		if entry, err := info(); err == nil && os.SameFile(store.info, entry) {
			return true
		}
	}
	return false
}

// CheckRead applies the credential check to one path. File tools use it for
// every resolved target, including edits, whichever root admitted the path.
func CheckRead(workspace, path string) error {
	return NewCredentialGuard(workspace).Check(path)
}

// spelledWithin compares the paths as written, without resolving links.
func spelledWithin(root, path string) bool {
	root, path = filepath.Clean(root), filepath.Clean(path)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
