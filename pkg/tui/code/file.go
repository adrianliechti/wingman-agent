package code

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	toolfs "github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
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

	gitignore := toolfs.NewGitignore(context.Background(), fsys)
	ignored := func(path string, isDir bool) bool {
		ignored, _ := gitignore.Ignored(filepath.ToSlash(path), isDir)
		return ignored
	}

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

			if name != "." && ignored(path, true) {
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		if ignored(path, false) {
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

// contextFileID normalizes to slashes so pending paths (possibly OS-native)
// and workspace walk paths (always slash) produce the same picker ID.
func contextFileID(path string) string {
	return "file:" + filepath.ToSlash(path)
}

// applyContextSelection replaces the pending attachments with the picker
// outcome. Pending files the picker never offered (paths outside the
// workspace, ignored files) ride along in the selection and are kept.
func (a *App) applyContextSelection(images []harness.Content, files []fileMatch, ids []string) {
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}

	var content []harness.Content
	for _, existing := range a.pendingContent {
		if existing.File == nil {
			content = append(content, existing)
		}
	}
	for i, image := range images {
		if selected[fmt.Sprintf("image:%d", i)] {
			content = append(content, image)
		}
	}

	offered := make(map[string]bool, len(files))
	var selectedFiles []string
	for _, f := range files {
		id := contextFileID(f.Path)
		offered[id] = true
		if selected[id] {
			selectedFiles = append(selectedFiles, f.Path)
		}
	}
	for _, path := range a.pendingFiles {
		if id := contextFileID(path); !offered[id] && selected[id] {
			selectedFiles = append(selectedFiles, path)
		}
	}

	a.pendingContent = content
	a.pendingFiles = selectedFiles
}

func (a *App) addFileToContext(path string) error {
	if slices.Contains(a.pendingFiles, path) {
		return nil
	}
	a.pendingFiles = append(a.pendingFiles, path)

	return nil
}

func (a *App) attachmentLines(width int) []string {
	images := a.countPendingImages()
	total := images + len(a.pendingFiles)
	if total == 0 {
		return nil
	}

	t := theme.Default
	chip := func(label string) string {
		return surfaceLine(colored(t.Cyan, " "+label+" "), ansi.Width(label)+2, t.Surface)
	}

	var labels []string
	for i := range images {
		labels = append(labels, fmt.Sprintf("image %d", i+1))
	}
	for _, path := range a.pendingFiles {
		labels = append(labels, filepath.Base(path))
	}

	prefix := cellIndent + " "
	line := prefix
	shown := 0
	for i, label := range labels {
		remaining := len(labels) - i - 1
		candidate := chip(ansi.Truncate(label, 24, "…"))
		extra := ""
		if line != prefix {
			extra = " "
		}
		more := ""
		if remaining > 0 {
			more = " " + dim(fmt.Sprintf("+%d", remaining))
		}
		if ansi.Width(line+extra+candidate+more) > width {
			break
		}
		line += extra + candidate
		shown++
	}
	if shown < len(labels) {
		count := dim(fmt.Sprintf("+%d more", len(labels)-shown))
		if ansi.Width(line+" "+count) <= width {
			line += " " + count
		}
	}
	return []string{ansi.Truncate(line, width, "…")}
}
