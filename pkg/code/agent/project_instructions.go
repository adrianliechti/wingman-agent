package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const projectInstructionsMaxBytes = 25 * 1024

type projectInstructionsEntry struct {
	path, rel string
	info      os.FileInfo
	err       error
}

type projectInstructionFile struct {
	info    os.FileInfo // Last successful read; nil if the source has never loaded.
	content string
	warning string
}

// Cache each source independently: one unreadable file must not erase its
// guidance or prevent confirmed edits and deletions in other sources.
type projectInstructionCache struct {
	files map[string]projectInstructionFile
}

func (c *projectInstructionCache) load(wd string) (string, []string) {
	result, files, warnings := renderProjectInstructions(findProjectInstructions(wd), c.files)
	c.files = files
	return result, warnings
}

func findProjectInstructions(wd string) []projectInstructionsEntry {
	wd = filepath.Clean(wd)
	var groups [][]projectInstructionsEntry
	for dir := wd; ; {
		var group []projectInstructionsEntry
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			p := filepath.Join(dir, name)
			info, err := os.Stat(p)
			if os.IsNotExist(err) {
				continue
			}
			rel, _ := filepath.Rel(wd, p)
			group = append(group, projectInstructionsEntry{path: p, rel: rel, info: info, err: err})
		}
		groups = append(groups, group)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Broader guidance first, most-specific last.
	var found []projectInstructionsEntry
	for _, group := range slices.Backward(groups) {
		found = append(found, group...)
	}
	return found
}

func renderProjectInstructions(entries []projectInstructionsEntry, previous map[string]projectInstructionFile) (string, map[string]projectInstructionFile, []string) {
	parts := make([]string, 0, len(entries))
	files := make(map[string]projectInstructionFile, len(entries))
	seen := make(map[string]bool, len(entries))
	var warnings []string
	for _, e := range entries {
		cached := previous[e.path]
		err := e.err
		if err == nil && (cached.info == nil || cached.warning != "" || !cached.info.ModTime().Equal(e.info.ModTime()) || cached.info.Size() != e.info.Size()) {
			var data []byte
			data, err = os.ReadFile(e.path)
			if err == nil {
				cached = projectInstructionFile{info: e.info, content: strings.TrimSpace(string(data))}
			}
		}
		if os.IsNotExist(err) {
			// The source disappeared between discovery and reading.
			continue
		}
		if err != nil {
			warning := fmt.Sprintf("could not refresh instructions from %s: %v", e.path, err)
			if cached.info != nil {
				warning += "; keeping the last successfully loaded content"
			}
			if warning != cached.warning {
				warnings = append(warnings, warning)
			}
			cached.warning = warning
		}
		files[e.path] = cached
		if cached.content == "" || seen[cached.content] {
			continue
		}
		seen[cached.content] = true
		parts = append(parts, fmt.Sprintf("From %s:\n\n%s", e.rel, cached.content))
	}

	// Over budget, drop the broadest guidance first.
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	omitted := 0
	for len(parts) > 1 && total > projectInstructionsMaxBytes {
		total -= len(parts[0])
		parts = parts[1:]
		omitted++
	}
	result := strings.Join(parts, "\n\n---\n\n")
	if omitted > 0 {
		result = fmt.Sprintf("[%d broader instruction file(s) omitted — over the %dKB budget]\n\n%s", omitted, projectInstructionsMaxBytes/1024, result)
	}
	if len(result) > projectInstructionsMaxBytes {
		result = result[:projectInstructionsMaxBytes] + "\n\n[truncated]"
	}
	return result, files, warnings
}

func (s *sessionState) projectInstructions() string {
	s.projectInstructionsMu.Lock()
	defer s.projectInstructionsMu.Unlock()
	result, warnings := s.projectInstructionCache.load(s.parent.workspace.RootPath)
	for _, warning := range warnings {
		fmt.Fprintln(os.Stderr, "warning: "+warning)
	}
	return result
}
