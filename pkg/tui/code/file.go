package code

import (
	"bufio"
	"io/fs"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

var defaultIgnoreDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".svn":         true,
	"__pycache__":  true,
	".venv":        true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
}

type fileMatch struct {
	Path string
	Name string
}

func (a *App) collectFiles() []fileMatch {
	var files []fileMatch
	fsys := a.agent.Workspace().Root.FS()

	var allPatterns []gitignore.Pattern
	allPatterns = append(allPatterns, loadGitignore(fsys, nil)...)
	matcher := gitignore.NewMatcher(allPatterns)

	fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if d.IsDir() {
			name := d.Name()

			if name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}

			if defaultIgnoreDirs[name] {
				return filepath.SkipDir
			}

			relPath := filepath.ToSlash(path)
			pathParts := strings.Split(relPath, "/")

			if matcher.Match(pathParts, true) {
				return filepath.SkipDir
			}

			newPatterns := loadGitignore(fsys, strings.Split(path, "/"))

			if len(newPatterns) > 0 {
				allPatterns = append(allPatterns, newPatterns...)
				matcher = gitignore.NewMatcher(allPatterns)
			}

			return nil
		}

		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		relPath := filepath.ToSlash(path)
		pathParts := strings.Split(relPath, "/")

		if matcher.Match(pathParts, false) {
			return nil
		}

		files = append(files, fileMatch{
			Path: path,
			Name: d.Name(),
		})

		return nil
	})

	return files
}

func loadGitignore(fsys fs.FS, domain []string) []gitignore.Pattern {
	gitignorePath := ".gitignore"

	if len(domain) > 0 {
		gitignorePath = pathpkg.Join(append(domain, ".gitignore")...)
	}

	f, err := fsys.Open(gitignorePath)

	if err != nil {
		return nil
	}
	defer f.Close()

	var patterns []gitignore.Pattern
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		patterns = append(patterns, gitignore.ParsePattern(line, domain))
	}

	return patterns
}

func (a *App) addFileToContext(path string) error {
	a.pendingFiles = append(a.pendingFiles, path)

	return nil
}
