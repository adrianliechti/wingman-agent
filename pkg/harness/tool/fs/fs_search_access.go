package fs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
)

// All search content and directory listings pass through Open, including
// .gitignore. Stat stays metadata-only so special files need never be opened.
type guardedSearchFS struct {
	root  *os.Root
	guard sandboxpolicy.CredentialGuard
}

func (g guardedSearchFS) Stat(name string) (fs.FileInfo, error) {
	return fs.Stat(g.root.FS(), name)
}

func (g guardedSearchFS) Open(name string) (fs.File, error) {
	path, err := filepath.Localize(name)
	if err != nil {
		return nil, err
	}
	f, err := g.root.OpenFile(path, readFlags, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.IsDir() && !info.Mode().IsRegular() {
		err = fmt.Errorf("cannot search special file %q", name)
	}
	if err == nil {
		err = g.guard.CheckOpened(f, info)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
