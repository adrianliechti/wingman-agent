package rewind

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

func (m *Manager) Restore(hash string) error {
	if hash == "" {
		return errors.New("empty hash")
	}

	if err := m.ready(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}

	target := plumbing.NewHash(hash)

	commit, err := m.repo.CommitObject(target)
	if err != nil {
		return fmt.Errorf("failed to get commit: %w", err)
	}

	targetTree, err := commit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get commit tree: %w", err)
	}

	root, err := m.snapshot()
	if err != nil {
		return fmt.Errorf("failed to snapshot working tree: %w", err)
	}

	liveTree, err := object.GetTree(m.storage, root)
	if err != nil {
		return fmt.Errorf("failed to get snapshot tree: %w", err)
	}

	changes, err := object.DiffTree(liveTree, targetTree)
	if err != nil {
		return fmt.Errorf("failed to compute diff: %w", err)
	}

	var upserts []*object.Change

	for _, change := range changes {
		action, err := change.Action()
		if err != nil {
			return fmt.Errorf("failed to resolve change: %w", err)
		}
		if action == merkletrie.Delete {
			m.removePath(change.From.Name)
		} else {
			upserts = append(upserts, change)
		}
	}

	for _, change := range upserts {
		if err := m.materialize(change.To.Name, change.To.TreeEntry); err != nil {
			return fmt.Errorf("failed to restore %s: %w", change.To.Name, err)
		}
	}

	if err := m.setHead(target); err != nil {
		return err
	}

	m.baselineHash = target
	return nil
}

func (m *Manager) removePath(name string) {
	delete(m.manifest, name)

	if err := os.Remove(m.absPath(name)); err != nil {
		return
	}

	for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if os.Remove(m.absPath(dir)) != nil {
			break
		}
	}
}

func (m *Manager) materialize(name string, entry object.TreeEntry) error {
	if entry.Mode == filemode.Submodule {
		m.manifest[name] = manifestEntry{mode: filemode.Submodule, hash: entry.Hash}
		return nil
	}

	blob, err := object.GetBlob(m.storage, entry.Hash)
	if err != nil {
		return err
	}

	r, err := blob.Reader()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return err
	}

	abs := m.absPath(name)

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	os.Remove(abs)

	if entry.Mode == filemode.Symlink {
		if err := os.Symlink(string(data), abs); err != nil {
			return err
		}
	} else {
		perm := os.FileMode(0o644)
		if entry.Mode == filemode.Executable {
			perm = 0o755
		}
		if err := os.WriteFile(abs, data, perm); err != nil {
			return err
		}
		os.Chmod(abs, perm)
	}

	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}

	m.manifest[name] = manifestEntry{
		size:  info.Size(),
		mtime: info.ModTime().UnixNano(),
		mode:  entry.Mode,
		hash:  entry.Hash,
	}
	return nil
}
