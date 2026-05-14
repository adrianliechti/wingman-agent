package lsp

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Each (dir, server-command) pair gets its own root — multiple project types
// at the same dir produce separate entries.
type projectRoot struct {
	Dir    string
	Server Server
}

var ignoredDirs = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	"target":       true,
	"build":        true,
	"dist":         true,
	".next":        true,
	".nuxt":        true,
}

// projectBinDirs are per-ecosystem subdirectories probed in order at each level of the walk;
// first match wins.
var projectBinDirs = []string{
	filepath.Join("node_modules", ".bin"),
	filepath.Join(".venv", "bin"),
	filepath.Join("venv", "bin"),
	filepath.Join("vendor", "bin"),
}

func hasFileMatching(fsys fs.FS, relDir string, patterns []string) bool {
	prefix := ""
	if relDir != "" && relDir != "." {
		prefix = filepath.ToSlash(relDir) + "/"
	}
	for _, pat := range patterns {
		matches, err := doublestar.Glob(fsys, prefix+"**/"+pat)
		if err == nil && len(matches) > 0 {
			return true
		}
	}
	return false
}

// resolveCommand returns the absolute path of command if it lives under one of projectBinDirs
// between dir and workingDir (inclusive); empty string means caller falls back to exec.LookPath.
func resolveCommand(dir, workingDir, command string) string {
	cur := filepath.Clean(dir)
	root := filepath.Clean(workingDir)
	for {
		for _, sub := range projectBinDirs {
			candidate := filepath.Join(cur, sub, command)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return candidate
			}
		}
		if cur == root || !isSubPath(root, cur) {
			return ""
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func detectAll(workingDir string) []projectRoot {
	var roots []projectRoot
	seen := make(map[string]bool)
	resolveCache := make(map[string]string)

	fsys := filteredFS{root: workingDir}

	for _, pt := range knownProjects {
		for _, marker := range pt.Markers {
			matches, err := doublestar.Glob(fsys, "**/"+marker)
			if err != nil {
				continue
			}

			for _, match := range matches {
				dir := filepath.Join(workingDir, filepath.Dir(match))

				if excluded(dir, pt.Excludes) {
					continue
				}

				if len(pt.Requires) > 0 {
					relDir, err := filepath.Rel(workingDir, dir)
					if err != nil {
						continue
					}
					if !hasFileMatching(fsys, relDir, pt.Requires) {
						continue
					}
				}

				for _, candidate := range pt.Servers {
					key := dir + "\x00" + candidate.Command
					if seen[key] {
						continue
					}
					seen[key] = true

					path, cached := resolveCache[key]
					if !cached {
						if abs := resolveCommand(dir, workingDir, candidate.Command); abs != "" {
							path = abs
						} else if _, err := exec.LookPath(candidate.Command); err == nil {
							path = candidate.Command
						}
						resolveCache[key] = path
					}
					if path == "" {
						continue
					}

					server := candidate
					server.Command = path
					roots = append(roots, projectRoot{Dir: dir, Server: server})
					break
				}
			}
		}
	}

	return roots
}

func excluded(dir string, excludes []string) bool {
	for _, marker := range excludes {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

// filteredFS wraps os.DirFS but skips ignoredDirs and dot-directories.
type filteredFS struct {
	root string
}

func (f filteredFS) Open(name string) (fs.File, error) {
	return os.Open(filepath.Join(f.root, name))
}

func (f filteredFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(filepath.Join(f.root, name))
	if err != nil {
		return nil, err
	}

	filtered := entries[:0]
	for _, e := range entries {
		if e.IsDir() && (ignoredDirs[e.Name()] || strings.HasPrefix(e.Name(), ".")) {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered, nil
}

func (f filteredFS) Stat(name string) (fs.FileInfo, error) {
	return os.Stat(filepath.Join(f.root, name))
}

func isSubPath(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)

	if parent == child {
		return true
	}

	if !strings.HasSuffix(parent, string(filepath.Separator)) {
		parent += string(filepath.Separator)
	}

	return strings.HasPrefix(child, parent)
}
