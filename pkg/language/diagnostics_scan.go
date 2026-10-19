package language

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

const (
	sourceDiscoveryCountLimit = 2000
	serverIdleTimeout         = 15 * time.Second
	serverWarmupWindow        = 30 * time.Second
)

var diagnosticSkippedDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
	"venv":         true,
	"target":       true,
	"build":        true,
	"dist":         true,
}

func (s *Service) Diagnostics(ctx context.Context) WorkspaceReport {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed {
		return WorkspaceReport{}
	}
	return collectWorkspaceDiagnostics(ctx, s.manager)
}

func (s *Service) FileDiagnostics(ctx context.Context, filePath string, content *string) ([]Diagnostic, bool, error) {
	var raw []lsp.Diagnostic
	var known bool
	err := s.withLSPDocument(ctx, filePath, content, func(session *lsp.Session, uri string) error {
		raw, known = session.WaitForDiagnostics(ctx, uri, 2*time.Second)
		return nil
	})
	return DiagnosticsFromProtocol(raw), known, err
}

func (s *Service) PostEditDiagnostics(ctx context.Context, filePath string) string {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed || !s.hasLSPServerFor(filePath) {
		return ""
	}
	session, ok := s.manager.ActiveSession(filePath)
	if !ok {
		s.manager.WarmUpFile(filePath)
		return ""
	}
	uri, err := session.OpenDocument(ctx, filePath)
	if err != nil {
		return ""
	}
	raw, known := session.WaitForDiagnostics(ctx, uri, 2*time.Second)
	if !known {
		return ""
	}
	values := DiagnosticsFromProtocol(raw)
	errors := values[:0]
	for _, value := range values {
		if value.Severity == SeverityError || value.Severity == 0 {
			errors = append(errors, value)
		}
	}
	if len(errors) == 0 {
		return ""
	}
	displayPath := relativeDiagnosticPath(s.root, filePath)
	var result strings.Builder
	fmt.Fprintf(&result, "lsp diagnostics for %s (%s):\n", displayPath, DiagnosticSummary(errors))
	shown := errors
	if len(shown) > 10 {
		shown = shown[:10]
	}
	for _, value := range shown {
		fmt.Fprintf(&result, "  %s\n", FormatDiagnosticLine(displayPath, value))
	}
	if len(errors) > len(shown) {
		fmt.Fprintf(&result, "  ... and %d more\n", len(errors)-len(shown))
	}
	return strings.TrimRight(result.String(), "\n")
}

func collectWorkspaceDiagnostics(ctx context.Context, manager *lsp.Manager) WorkspaceReport {
	projects := manager.Projects()
	reports := make([]WorkspaceReport, len(projects))
	var wg sync.WaitGroup

	for i, project := range projects {
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() { reports[i] = collectProjectDiagnostics(ctx, manager, project, projects) })
	}
	wg.Wait()

	report := WorkspaceReport{Diagnostics: make(map[string][]Diagnostic)}
	for _, project := range reports {
		maps.Copy(report.Diagnostics, project.Diagnostics)
		report.CheckedFiles += project.CheckedFiles
		report.DiscoveredFiles += project.DiscoveredFiles
		report.UnknownFiles += project.UnknownFiles
		report.DiscoveryTruncated = report.DiscoveryTruncated || project.DiscoveryTruncated
		report.Analyzing = report.Analyzing || project.Analyzing
		report.UnavailableServers = append(report.UnavailableServers, project.UnavailableServers...)
	}
	slices.Sort(report.UnavailableServers)
	report.UnavailableServers = slices.Compact(report.UnavailableServers)
	return report
}

func collectProjectDiagnostics(ctx context.Context, manager *lsp.Manager, project lsp.Project, projects []lsp.Project) WorkspaceReport {
	if ctx.Err() != nil {
		return WorkspaceReport{}
	}
	session, err := manager.ProjectSession(ctx, project)
	if err != nil {
		return WorkspaceReport{UnavailableServers: []string{projectLabel(manager.WorkingDir(), project)}}
	}
	files, uris, total, truncated := openDiagnosticFiles(ctx, project, projects, session)
	idle := waitForServerIdle(ctx, session, serverIdleTimeout)
	waitForPushedDiagnostics(ctx, session, uris, min(30*time.Second, 5*time.Second+time.Duration(len(uris))*20*time.Millisecond))
	states := collectDiagnosticStates(ctx, session, uris)

	report := WorkspaceReport{
		Diagnostics:        make(map[string][]Diagnostic),
		CheckedFiles:       len(files),
		DiscoveredFiles:    total,
		DiscoveryTruncated: truncated,
		Analyzing:          !idle || session.Age() < serverWarmupWindow,
	}
	for i, file := range files {
		if !states[i].known {
			report.UnknownFiles++
			continue
		}
		if values := DiagnosticsFromProtocol(states[i].diagnostics); len(values) > 0 {
			report.Diagnostics[file] = values
		}
	}
	return report
}

func openDiagnosticFiles(ctx context.Context, project lsp.Project, projects []lsp.Project, session *lsp.Session) ([]string, []string, int, bool) {
	projectID := diagnosticProjectKey(project)
	files, total, truncated := discoverDiagnosticFiles(ctx, project.Dir, project.Server.Languages, sourceDiscoveryCountLimit, func(path string) bool {
		owner := diagnosticProject(projects, path)
		return owner != nil && diagnosticProjectKey(*owner) == projectID
	})
	opened := make([]string, 0, len(files))
	uris := make([]string, 0, len(files))
	for _, file := range files {
		if ctx.Err() != nil {
			break
		}
		uri, err := session.OpenDocument(ctx, file)
		if err == nil {
			opened = append(opened, file)
			uris = append(uris, uri)
		}
	}
	return opened, uris, total, truncated
}

func diagnosticProject(projects []lsp.Project, filePath string) *lsp.Project {
	extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(filePath), "."))
	dir := filepath.Dir(filePath)
	var best *lsp.Project
	for _, project := range projects {
		if !pathWithin(project.Dir, dir) || !slices.Contains(project.Server.Languages, extension) || best != nil && len(project.Dir) <= len(best.Dir) {
			continue
		}
		candidate := project
		best = &candidate
	}
	return best
}

func diagnosticProjectKey(project lsp.Project) string {
	return filepath.Clean(project.Dir) + "\x00" + project.Server.Command + "\x00" + strings.Join(project.Server.Args, "\x00")
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func projectLabel(root string, project lsp.Project) string {
	if rel := relativeDiagnosticPath(root, project.Dir); rel != "." {
		return fmt.Sprintf("%s (%s)", project.Server.Name, rel)
	}
	return project.Server.Name
}

func waitForServerIdle(ctx context.Context, session *lsp.Session, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if !session.Analyzing() && session.Age() >= 3*time.Second {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !session.Analyzing()
}

func waitForPushedDiagnostics(ctx context.Context, session *lsp.Session, uris []string, timeout time.Duration) {
	if session.SupportsPullDiagnostics() || len(uris) == 0 {
		return
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	lastSeen := -1
	quietTicks := 0
	for {
		pending := 0
		for _, uri := range uris {
			if !session.PushSeen(uri) {
				pending++
			}
		}
		if pending == 0 {
			return
		}
		seen := len(uris) - pending
		if seen != lastSeen {
			lastSeen = seen
			quietTicks = 0
		} else if !session.Analyzing() {
			quietTicks++
			if (seen > 0 && quietTicks >= 3) || (session.Age() >= 3*time.Second && quietTicks >= 5) {
				return
			}
		} else {
			quietTicks = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

type diagnosticState struct {
	diagnostics []lsp.Diagnostic
	known       bool
}

func collectDiagnosticStates(ctx context.Context, session *lsp.Session, uris []string) []diagnosticState {
	states := make([]diagnosticState, len(uris))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(8, len(uris)) {
		workers.Go(func() {
			for i := range jobs {
				states[i].diagnostics, states[i].known = session.DiagnosticsState(ctx, uris[i])
			}
		})
	}
dispatch:
	for i := range uris {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	return states
}

func discoverDiagnosticFiles(ctx context.Context, root string, extensions []string, maxFiles int, include func(string) bool) ([]string, int, bool) {
	extensionSet := make(map[string]bool, len(extensions))
	for _, extension := range extensions {
		extensionSet["."+strings.ToLower(extension)] = true
	}
	var files []string
	total := 0
	truncated := false
	pathutil.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || diagnosticSkippedDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if extensionSet[strings.ToLower(filepath.Ext(path))] && (include == nil || include(path)) {
			total++
			if total > sourceDiscoveryCountLimit {
				total = sourceDiscoveryCountLimit
				truncated = true
				return filepath.SkipAll
			}
			if len(files) < maxFiles {
				files = append(files, path)
			}
		}
		return nil
	})
	return files, total, truncated
}
