package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

const (
	projectInstructionsMaxBytes  = 25 * 1024
	projectInstructionsSeparator = "\n\n---\n\n"
)

type projectInstructionsEntry struct {
	path, rel string
	info      os.FileInfo
	err       error
}

type projectInstructionFile struct {
	info    os.FileInfo // Last successful read; nil if the source has never loaded.
	content string
	warning string
	// budget is the omission or truncation last reported for this source.
	budget string
}

// Cache each source independently: one unreadable file must not erase its
// guidance or prevent confirmed edits and deletions in other sources.
type projectInstructionCache struct {
	files map[string]projectInstructionFile
}

// load returns the instructions and the warnings that are new since the last
// load. A warning repeats only once its text changes or its cause has cleared.
func (c *projectInstructionCache) load(wd string) (string, []string) {
	result, files, warnings := renderProjectInstructions(findProjectInstructions(wd), c.files)
	c.files = files
	return result, warnings
}

func findProjectInstructions(wd string) []projectInstructionsEntry {
	wd = filepath.Clean(wd)
	credentials := sandboxpolicy.NewCredentialGuard(wd)
	var groups [][]projectInstructionsEntry
	for dir := wd; ; {
		var group []projectInstructionsEntry
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			p := filepath.Join(dir, name)
			info, err := os.Stat(p)
			if os.IsNotExist(err) {
				continue
			}
			// The repository controls these files and their content reaches the
			// prompt, so a link into a credential store is not followed.
			if err == nil {
				err = credentials.Check(p)
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

type projectInstructionPart struct {
	path, rel, content string
}

func (p projectInstructionPart) header() string {
	return fmt.Sprintf("From %s:\n\n", p.rel)
}

func renderProjectInstructions(entries []projectInstructionsEntry, previous map[string]projectInstructionFile) (string, map[string]projectInstructionFile, []string) {
	parts := make([]projectInstructionPart, 0, len(entries))
	files := make(map[string]projectInstructionFile, len(entries))
	seen := make(map[string]bool, len(entries))
	var warnings []string
	for _, e := range entries {
		cached := previous[e.path]
		err := e.err
		if denied, ok := errors.AsType[*sandboxpolicy.CredentialError](err); ok {
			warning := fmt.Sprintf("skipped instructions from %s: it resolves into protected credential location %s", e.path, denied.Store)
			if warning != cached.warning {
				warnings = append(warnings, warning)
			}
			// Nothing read through the link is kept, not even earlier content.
			files[e.path] = projectInstructionFile{warning: warning}
			continue
		}
		if err == nil && (cached.info == nil || cached.warning != "" || !cached.info.ModTime().Equal(e.info.ModTime()) || cached.info.Size() != e.info.Size()) {
			var data []byte
			data, err = os.ReadFile(e.path)
			if err == nil {
				cached = projectInstructionFile{info: e.info, content: strings.TrimSpace(string(data)), budget: cached.budget}
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
		parts = append(parts, projectInstructionPart{path: e.path, rel: e.rel, content: cached.content})
	}

	result, budget := fitProjectInstructions(parts)
	for _, e := range entries {
		file, ok := files[e.path]
		if !ok {
			continue
		}
		if warning := budget[e.path]; warning != "" && warning != file.budget {
			warnings = append(warnings, warning)
		}
		file.budget = budget[e.path]
		files[e.path] = file
	}
	return result, files, warnings
}

// fitProjectInstructions renders parts within the byte budget. It drops the
// broadest guidance first, then shortens the one remaining file at a line
// boundary. Each affected source gets a user-facing warning, keyed by its path.
func fitProjectInstructions(parts []projectInstructionPart) (string, map[string]string) {
	budget := map[string]string{}
	omitted := 0
	render := func() string {
		var b strings.Builder
		if omitted > 0 {
			fmt.Fprintf(&b, "[%d broader instruction file(s) omitted — over the %dKB budget]\n\n", omitted, projectInstructionsMaxBytes/1024)
		}
		for i, part := range parts {
			if i > 0 {
				b.WriteString(projectInstructionsSeparator)
			}
			b.WriteString(part.header())
			b.WriteString(part.content)
		}
		return b.String()
	}

	result := render()
	for len(parts) > 1 && len(result) > projectInstructionsMaxBytes {
		budget[parts[0].path] = fmt.Sprintf("Skipped %s (%d bytes): project instructions exceed the %d KB budget, and more specific files take precedence.", parts[0].path, len(parts[0].content), projectInstructionsMaxBytes/1024)
		parts = parts[1:]
		omitted++
		result = render()
	}
	if len(result) <= projectInstructionsMaxBytes {
		return result, budget
	}

	last := &parts[0]
	size := len(last.content)
	limit := max(0, size-(len(result)-projectInstructionsMaxBytes))
	// Cut at a line end. A line too long for that to keep most of the budget
	// is cut at a rune boundary instead; neither splits a character.
	kept := text.HeadLines(last.content, limit)
	if len(kept) < limit/2 {
		kept = text.HeadBytes(last.content, limit)
	}
	last.content = strings.TrimRight(kept, " \t\r\n")
	budget[last.path] = fmt.Sprintf("Loaded only %d of %d bytes from %s: project instructions are limited to %d KB. Trim the file to load the rest.", len(last.content), size, last.path, projectInstructionsMaxBytes/1024)
	return render() + "\n\n[truncated]", budget
}

func (s *sessionState) projectInstructions() string {
	s.projectInstructionsMu.Lock()
	result, warnings := s.projectInstructionCache.load(s.parent.workspace.RootPath)
	s.projectInstructionsMu.Unlock()
	for _, warning := range warnings {
		s.parent.warn(s.id, warning)
	}
	return result
}

// warningUI is implemented by front ends that can show a non-fatal warning in
// a session's transcript.
type warningUI interface {
	Warn(sessionID, message string)
}

// warn reports a problem the user should know about without failing the turn.
// Without a front end that can show it, the warning goes to stderr.
func (a *Agent) warn(sessionID, message string) {
	if ui, ok := a.currentUI().(warningUI); ok {
		ui.Warn(sessionID, message)
		return
	}
	fmt.Fprintln(os.Stderr, "warning: "+message)
}
