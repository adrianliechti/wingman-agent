package rewind

import (
	"bytes"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type manifestEntry struct {
	size  int64
	mtime int64
	mode  filemode.FileMode
	hash  plumbing.Hash
}

const racyWindow = int64(2 * time.Second)

func (m *Manager) snapshot() (plumbing.Hash, error) {
	res := m.scan()
	resolved, pending, dirty := m.plan(res)

	if dirty {
		m.refreshExcludes()
		res = m.scan()
		resolved, pending, _ = m.plan(res)
	}

	m.hashPending(resolved, pending)
	m.carryHidden(resolved)

	root, err := m.writeTrees(resolved)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	m.manifest = resolved
	m.floor = res.start.UnixNano()

	return root, nil
}

func (m *Manager) plan(res *scanResult) (map[string]manifestEntry, []scanEntry, bool) {
	resolved := make(map[string]manifestEntry, len(res.entries))
	seen := make(map[string]struct{}, len(res.entries))

	var pending []scanEntry
	var dirty bool

	for _, e := range res.entries {
		seen[e.path] = struct{}{}

		old, ok := m.manifest[e.path]
		if ok && old.size == e.size && old.mtime == e.mtime && old.mode == e.mode && e.mtime+racyWindow < m.floor {
			resolved[e.path] = old
			continue
		}

		if path.Base(e.path) == ".gitignore" {
			dirty = true
		}
		pending = append(pending, e)
	}

	for _, sub := range res.submodules {
		if old, ok := m.manifest[sub]; ok && old.mode == filemode.Submodule {
			resolved[sub] = old
			seen[sub] = struct{}{}
		}
	}

	for p := range m.manifest {
		if _, ok := seen[p]; ok {
			continue
		}
		if path.Base(p) == ".gitignore" {
			dirty = true
		}
	}

	return resolved, pending, dirty
}

func (m *Manager) carryHidden(resolved map[string]manifestEntry) {
	matcher := m.excludeMatcher()

	for p, old := range m.manifest {
		if _, ok := resolved[p]; ok {
			continue
		}
		if old.mode == filemode.Submodule {
			continue
		}
		if !matcher.Match(strings.Split(p, "/"), false) {
			continue
		}
		if _, err := os.Lstat(m.absPath(p)); err != nil {
			continue
		}
		resolved[p] = old
	}
}

func (m *Manager) hashPending(resolved map[string]manifestEntry, pending []scanEntry) {
	if len(pending) == 0 {
		return
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())

	for _, e := range pending {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			hash, err := m.writeBlob(e)
			if err != nil {
				return
			}

			mu.Lock()
			resolved[e.path] = manifestEntry{size: e.size, mtime: e.mtime, mode: e.mode, hash: hash}
			mu.Unlock()
		}()
	}

	wg.Wait()
}

// isText reports whether data looks like text, using git's heuristic: content
// with a NUL byte in the first 8000 bytes is treated as binary.
func isText(data []byte) bool {
	if len(data) > 8000 {
		data = data[:8000]
	}
	return bytes.IndexByte(data, 0) < 0
}

func (m *Manager) writeBlob(e scanEntry) (plumbing.Hash, error) {
	abs := m.absPath(e.path)

	var data []byte

	if e.mode == filemode.Symlink {
		target, err := os.Readlink(abs)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		data = []byte(target)
	} else {
		var err error
		if data, err = os.ReadFile(abs); err != nil {
			return plumbing.ZeroHash, err
		}
		if m.crlf && isText(data) {
			data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		}
	}

	hash := plumbing.ComputeHash(plumbing.BlobObject, data)

	m.storeMu.Lock()
	defer m.storeMu.Unlock()

	if m.storage.Storer.HasEncodedObject(hash) == nil {
		return hash, nil
	}

	obj := m.storage.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(data)))

	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}

	return m.storage.SetEncodedObject(obj)
}

type treeNode struct {
	files map[string]manifestEntry
	dirs  map[string]*treeNode
}

func (m *Manager) writeTrees(entries map[string]manifestEntry) (plumbing.Hash, error) {
	root := &treeNode{}

	for p, e := range entries {
		parts := strings.Split(p, "/")
		node := root
		for _, part := range parts[:len(parts)-1] {
			if node.dirs == nil {
				node.dirs = map[string]*treeNode{}
			}
			child := node.dirs[part]
			if child == nil {
				child = &treeNode{}
				node.dirs[part] = child
			}
			node = child
		}
		if node.files == nil {
			node.files = map[string]manifestEntry{}
		}
		node.files[parts[len(parts)-1]] = e
	}

	return m.writeTree(root)
}

func (m *Manager) writeTree(node *treeNode) (plumbing.Hash, error) {
	entries := make([]object.TreeEntry, 0, len(node.files)+len(node.dirs))

	for name, child := range node.dirs {
		hash, err := m.writeTree(child)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
	}

	for name, e := range node.files {
		entries = append(entries, object.TreeEntry{Name: name, Mode: e.mode, Hash: e.hash})
	}

	sort.Sort(object.TreeEntrySorter(entries))

	tree := &object.Tree{Entries: entries}

	obj := m.storage.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}

	hash := obj.Hash()
	if m.storage.HasEncodedObject(hash) == nil {
		return hash, nil
	}

	return m.storage.SetEncodedObject(obj)
}
