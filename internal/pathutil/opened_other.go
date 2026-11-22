//go:build !linux && !darwin && !windows

package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// openedPath verifies the resolved spelling against the open file on hosts
// without a kernel handle-to-path implementation. Descend through pinned
// directories and reject any component replaced by a link during the lookup.
func openedPath(f *os.File) (string, error) {
	path, err := Resolve(f.Name())
	if err != nil {
		return "", err
	}
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	base := filepath.VolumeName(path) + string(filepath.Separator)
	dir, err := os.OpenRoot(base)
	if err != nil {
		return "", err
	}
	defer func() { dir.Close() }()
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return "", err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i, name := range parts {
		expected, err := dir.Lstat(name)
		if err != nil {
			return "", err
		}
		if i == len(parts)-1 {
			if os.SameFile(expected, opened) {
				return path, nil
			}
			break
		}
		if !expected.IsDir() {
			break
		}
		next, err := dir.OpenRoot(name)
		if err != nil {
			return "", err
		}
		dir.Close()
		dir = next
		actual, err := dir.Stat(".")
		if err != nil {
			return "", err
		}
		if !os.SameFile(expected, actual) {
			break
		}
	}
	return "", fmt.Errorf("path changed while checking %q", f.Name())
}
