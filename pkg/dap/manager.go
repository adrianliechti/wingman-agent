package dap

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/internal/tooling"
)

const detectionCacheTTL = 30 * time.Second

var (
	ErrActiveSession = errors.New("a debug session is already active")
	ErrBusy          = errors.New("debugger setup or launch is already in progress; try again when it finishes")
)

type detectedAdapter struct {
	adapter  AdapterDescriptor
	projects []string
}

type Manager struct {
	changed   chan struct{}
	root      string
	adapters  []AdapterDescriptor
	lookup    func(string) string
	start     sessionStarter
	terminal  TerminalLauncher
	connector AdapterConnector
	startMu   sync.Mutex

	detectMu   sync.Mutex
	detected   []detectedAdapter
	missing    []detectedAdapter
	detectedAt time.Time

	// breakpointMu orders breakpoint updates sent to adapters, so a later edit
	// is never overtaken by an earlier one.
	breakpointMu sync.Mutex

	mu               sync.Mutex
	session          *Session
	breakpoints      map[string][]SourceBreakpoint
	breakpointsAt    uint64
	breakpointEdits  map[string]uint64
	breakpointIDs    map[string]map[[2]int]int
	nextBreakpointID int
	resolved         map[int]ResolvedBreakpoint
	exceptionFilters []string
	exceptionsSet    bool
	plans            map[string]reviewedPlan
	closed           bool
}

type sessionStarter func(context.Context, string, Plan, StartOptions) (*Session, error)

func NewManager(root string, tools tooling.ManagedTools, adapters ...AdapterDescriptor) *Manager {
	resolve := func(string) string { return "" }
	if tools != nil {
		resolve = tools.Resolve
	}
	return newManager(root, adapters, resolve, startSession)
}

// SetTerminalLauncher connects the protocol client to the editor's PTY host.
// Passing nil disables terminal launches while leaving captured-output
// sessions available.
func (m *Manager) SetTerminalLauncher(launcher TerminalLauncher) {
	m.mu.Lock()
	m.terminal = launcher
	m.mu.Unlock()
}

// SetAdapterConnector installs the host bridge used by adapters such as Java,
// whose DAP socket is created by a language server rather than an executable.
func (m *Manager) SetAdapterConnector(connector AdapterConnector) {
	m.mu.Lock()
	m.connector = connector
	m.mu.Unlock()
}

// SetAdapters replaces descriptors used by future discovery and sessions.
// An already-running session is unaffected.
func (m *Manager) SetAdapters(adapters ...AdapterDescriptor) {
	m.detectMu.Lock()
	m.adapters = cloneAdapters(adapters)
	m.detected = nil
	m.missing = nil
	m.detectedAt = time.Time{}
	m.detectMu.Unlock()
}

// InvalidateDetection makes newly installed or updated adapters visible on
// the next lookup without waiting for the detection cache TTL.
func (m *Manager) InvalidateDetection() {
	m.detectMu.Lock()
	m.detectedAt = time.Time{}
	m.detectMu.Unlock()
}

func newManager(root string, adapters []AdapterDescriptor, lookup func(string) string, starter sessionStarter) *Manager {
	return &Manager{
		root:            filepath.Clean(root),
		adapters:        cloneAdapters(adapters),
		lookup:          lookup,
		start:           starter,
		breakpoints:     make(map[string][]SourceBreakpoint),
		breakpointEdits: make(map[string]uint64),
		breakpointIDs:   make(map[string]map[[2]int]int),
		resolved:        make(map[int]ResolvedBreakpoint),
	}
}

func (m *Manager) WorkingDir() string { return m.root }

func (m *Manager) detect(ctx context.Context) ([]detectedAdapter, error) {
	m.detectMu.Lock()
	defer m.detectMu.Unlock()
	if !m.detectedAt.IsZero() && time.Since(m.detectedAt) < detectionCacheTTL {
		return cloneDetected(m.detected), nil
	}

	specs := make([]tooling.ProjectSpec, len(m.adapters))
	for index, candidate := range m.adapters {
		specs[index] = tooling.ProjectSpec{Markers: candidate.Markers, Extensions: candidate.SourceExtensions}
	}
	projectsByAdapter, err := tooling.DetectProjects(ctx, m.root, specs)
	if err != nil {
		return nil, fmt.Errorf("detect debugger projects: %w", err)
	}
	var detected, missing []detectedAdapter
	for index, candidate := range m.adapters {
		projects := projectsByAdapter[index]
		if len(projects) == 0 {
			continue
		}
		command := m.lookup(candidate.Command)
		if command == "" {
			missing = append(missing, detectedAdapter{adapter: candidate, projects: projects})
			continue
		}
		candidate.Command = command
		detected = append(detected, detectedAdapter{adapter: candidate, projects: projects})
	}
	m.detected = detected
	m.missing = missing
	m.detectedAt = time.Now()
	return cloneDetected(detected), nil
}

// MissingRequirements reports debugger projects whose adapter command could
// not be resolved. It shares the normal discovery cache and workspace scan.
func (m *Manager) MissingRequirements(ctx context.Context) ([]AdapterRequirement, error) {
	if _, err := m.detect(ctx); err != nil {
		return nil, err
	}
	m.detectMu.Lock()
	defer m.detectMu.Unlock()
	result := make([]AdapterRequirement, 0, len(m.missing))
	for _, value := range m.missing {
		result = append(result, AdapterRequirement{
			Name: value.adapter.Name, Language: value.adapter.Language,
			Commands: []string{value.adapter.Command}, Projects: slices.Clone(value.projects),
		})
	}
	return result, nil
}

func cloneAdapters(values []AdapterDescriptor) []AdapterDescriptor {
	cloned := make([]AdapterDescriptor, len(values))
	for i, value := range values {
		cloned[i] = value
		cloned[i].Args = slices.Clone(value.Args)
		cloned[i].Markers = slices.Clone(value.Markers)
		cloned[i].SourceExtensions = slices.Clone(value.SourceExtensions)
		cloned[i].Defaults = maps.Clone(value.Defaults)
		cloned[i].ConfigurationPaths = slices.Clone(value.ConfigurationPaths)
		cloned[i].IOValues = maps.Clone(value.IOValues)
	}
	return cloned
}

func cloneDetected(values []detectedAdapter) []detectedAdapter {
	cloned := make([]detectedAdapter, len(values))
	for i, value := range values {
		cloned[i] = value
		cloned[i].adapter = cloneAdapters([]AdapterDescriptor{value.adapter})[0]
		cloned[i].projects = slices.Clone(value.projects)
	}
	return cloned
}

func (m *Manager) Adapters(ctx context.Context) ([]AdapterInfo, error) {
	values, err := m.detect(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]AdapterInfo, 0, len(values))
	for _, value := range values {
		result = append(result, AdapterInfo{
			Name:               value.adapter.Name,
			Language:           value.adapter.Language,
			Command:            value.adapter.Command,
			Projects:           slices.Clone(value.projects),
			ConfigurationPaths: slices.Clone(value.adapter.ConfigurationPaths),
			IOConfigKey:        value.adapter.IOConfigKey,
			TerminalStrategy:   value.adapter.TerminalStrategy,
		})
	}
	return result, nil
}

// BeginPreparation reserves an idle debugger until the returned release
// function is called. Tool updates share this reservation with Start because
// refreshing a hosted adapter can restart its language server.
func (m *Manager) BeginPreparation() (func(), error) {
	if !m.startMu.TryLock() {
		return nil, ErrBusy
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		m.startMu.Unlock()
		return nil, errors.New("DAP manager is closed")
	}
	if m.session != nil && m.session.Status().State != StateTerminated {
		m.startMu.Unlock()
		return nil, ErrActiveSession
	}
	return m.startMu.Unlock, nil
}

func (m *Manager) Start(ctx context.Context, options StartOptions) (*Session, error) {
	release, err := m.BeginPreparation()
	if err != nil {
		return nil, err
	}
	defer release()

	m.mu.Lock()
	for path, breakpoints := range options.Breakpoints {
		if err := validateSourceBreakpoints(breakpoints); err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("breakpoints for %s: %w", path, err)
		}
	}
	if err := validateFunctionBreakpoints(options.FunctionBreakpoints); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	planned := cloneSourceBreakpointSets(options.Breakpoints)
	options.Breakpoints, _ = m.combinedBreakpointsLocked(planned)
	options.terminalLauncher = m.terminal
	options.adapterConnector = m.connector
	options.hooks = sessionHooks{
		breakpointSource: func() (map[string][]SourceBreakpoint, uint64) {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.combinedBreakpointsLocked(planned)
		},
		exceptionFilters: func() ([]string, bool) {
			m.mu.Lock()
			defer m.mu.Unlock()
			return slices.Clone(m.exceptionFilters), m.exceptionsSet
		},
		breakpointID: func(path string, line, column int) int {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.breakpointIDs[filepath.Clean(path)][[2]int{line, column}]
		},
		onBreakpoint: m.resolveBreakpoint,
	}
	// A new session verifies every editor breakpoint again.
	for path, ids := range m.breakpointIDs {
		for position, id := range ids {
			m.resolved[id] = ResolvedBreakpoint{Path: path, Breakpoint: pendingBreakpoint(id, position[0], position[1])}
		}
	}
	m.notifyChangedLocked()
	m.mu.Unlock()

	values, err := m.detect(ctx)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		missing, missingErr := m.MissingRequirements(ctx)
		if missingErr != nil {
			return nil, missingErr
		}
		return nil, MissingAdapterError(missing)
	}
	selected, err := selectAdapter(values, options.Adapter)
	if err != nil {
		return nil, err
	}
	plan, err := resolvePlan(m.root, selected, options)
	if err != nil {
		return nil, err
	}
	registered := m.registeredAdapter(selected.adapter.Name)
	if registered == nil || registered.Command == "" {
		return nil, fmt.Errorf("debug adapter %s is not available for project %s", selected.adapter.Name, plan.ProjectDir)
	}
	plan.Adapter.Command = registered.Command
	plan.Adapter.Args = slices.Clone(registered.Args)
	if plan.Adapter.Transport == TransportConnect {
		if preparer, ok := options.adapterConnector.(AdapterPlanPreparer); ok {
			plan, err = preparer.PrepareAdapter(ctx, plan)
			if err != nil {
				return nil, fmt.Errorf("prepare debug adapter %s: %w", plan.Adapter.Name, err)
			}
		}
	}

	id := uuid.NewString()[:8]
	session, err := m.start(ctx, id, plan, options)
	if err != nil {
		if session != nil {
			m.mu.Lock()
			if m.closed {
				m.mu.Unlock()
				session.Close()
				return nil, errors.New("DAP manager is closed")
			}
			previous := m.session
			m.session = session
			m.notifyChangedLocked()
			m.mu.Unlock()
			if previous != nil {
				previous.Close()
			}
		}
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		session.Close()
		return nil, errors.New("DAP manager is closed")
	}
	previous := m.session
	m.session = session
	m.notifyChangedLocked()
	m.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	m.resyncBreakpoints(ctx, session)
	return session, nil
}

func (m *Manager) combinedBreakpointsLocked(planned map[string][]SourceBreakpoint) (map[string][]SourceBreakpoint, uint64) {
	combined := cloneSourceBreakpointSets(planned)
	for path, breakpoints := range m.breakpoints {
		path = filepath.Clean(path)
		combined[path] = mergeSourceBreakpoints(combined[path], breakpoints)
	}
	return combined, m.breakpointsAt
}

// resyncBreakpoints sends edits made after the session read the editor's
// breakpoints during configuration but before it became the active session.
func (m *Manager) resyncBreakpoints(ctx context.Context, session *Session) {
	m.breakpointMu.Lock()
	defer m.breakpointMu.Unlock()
	session.mu.Lock()
	since := session.configured
	session.mu.Unlock()
	m.mu.Lock()
	var paths []string
	for path, edited := range m.breakpointEdits {
		if edited > since {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	values := make([][]SourceBreakpoint, len(paths))
	for index, path := range paths {
		values[index] = slices.Clone(m.breakpoints[path])
	}
	m.mu.Unlock()
	for index, path := range paths {
		if resolved, err := session.SetBreakpoints(ctx, path, values[index]); err == nil {
			m.publishResolved(path, values[index], resolved)
		}
	}
}

func pendingBreakpoint(id, line, column int) Breakpoint {
	return Breakpoint{ID: id, Line: line, Column: column, Reason: "pending"}
}

// resolveBreakpoint records an adapter's verification of an editor breakpoint,
// identified by its requested position.
func (m *Manager) resolveBreakpoint(path string, line, column int, breakpoint Breakpoint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = filepath.Clean(path)
	id := m.breakpointIDs[path][[2]int{line, column}]
	if id == 0 {
		return
	}
	breakpoint.ID = id
	breakpoint.Reason = ""
	if breakpoint.Line == 0 {
		breakpoint.Line, breakpoint.Column = line, column
	}
	m.resolved[id] = ResolvedBreakpoint{Path: path, Breakpoint: breakpoint}
	m.notifyChangedLocked()
}

// publishResolved translates adapter results, which follow request order, to
// the editor's stable breakpoint ids.
func (m *Manager) publishResolved(path string, requested []SourceBreakpoint, resolved []Breakpoint) []Breakpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Breakpoint, 0, len(requested))
	for index, value := range requested {
		id := m.breakpointIDs[path][[2]int{value.Line, value.Column}]
		breakpoint := pendingBreakpoint(id, value.Line, value.Column)
		if index < len(resolved) {
			breakpoint = resolved[index]
			breakpoint.ID = id
			if breakpoint.Line == 0 {
				breakpoint.Line, breakpoint.Column = value.Line, value.Column
			}
		}
		if id != 0 {
			m.resolved[id] = ResolvedBreakpoint{Path: path, Breakpoint: breakpoint}
		}
		result = append(result, breakpoint)
	}
	m.notifyChangedLocked()
	return result
}

// ResolvedBreakpoints returns the latest verification of every editor
// breakpoint by its stable id.
func (m *Manager) ResolvedBreakpoints() map[int]ResolvedBreakpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.resolved)
}

// ResolvedSourceBreakpoints returns verification for one file in editor order.
func (m *Manager) ResolvedSourceBreakpoints(path string) []Breakpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = filepath.Clean(path)
	result := []Breakpoint{}
	for _, value := range m.breakpoints[path] {
		id := m.breakpointIDs[path][[2]int{value.Line, value.Column}]
		if resolved, ok := m.resolved[id]; ok {
			result = append(result, resolved.Breakpoint)
		} else {
			result = append(result, pendingBreakpoint(id, value.Line, value.Column))
		}
	}
	return result
}

func (m *Manager) registeredAdapter(name string) *AdapterDescriptor {
	m.detectMu.Lock()
	defer m.detectMu.Unlock()
	for _, adapter := range m.adapters {
		if strings.EqualFold(adapter.Name, name) {
			value := cloneAdapters([]AdapterDescriptor{adapter})[0]
			value.Command = m.lookup(value.Command)
			return &value
		}
	}
	return nil
}

// mergeSourceBreakpoints keeps a plan's initial stops and folds in the
// editor-owned set. At the same source position, the user's gutter breakpoint
// wins so its condition or log message is preserved.
func mergeSourceBreakpoints(planned, editor []SourceBreakpoint) []SourceBreakpoint {
	result := slices.Clone(planned)
	positions := make(map[[2]int]int, len(result))
	for index, breakpoint := range result {
		positions[[2]int{breakpoint.Line, breakpoint.Column}] = index
	}
	for _, breakpoint := range editor {
		position := [2]int{breakpoint.Line, breakpoint.Column}
		if index, exists := positions[position]; exists {
			result[index] = breakpoint
			continue
		}
		positions[position] = len(result)
		result = append(result, breakpoint)
	}
	slices.SortFunc(result, func(left, right SourceBreakpoint) int {
		if left.Line != right.Line {
			return left.Line - right.Line
		}
		return left.Column - right.Column
	})
	return result
}

func selectAdapter(values []detectedAdapter, requested string) (detectedAdapter, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" && requested != "auto" {
		for _, value := range values {
			if strings.EqualFold(value.adapter.Name, requested) {
				return value, nil
			}
		}
		var matching []detectedAdapter
		for _, value := range values {
			if strings.EqualFold(value.adapter.Language, requested) {
				matching = append(matching, value)
			}
		}
		if len(matching) == 1 {
			return matching[0], nil
		}
		if len(matching) > 1 {
			return detectedAdapter{}, fmt.Errorf("multiple %s debug adapters are available; choose one by name", requested)
		}
		return detectedAdapter{}, fmt.Errorf("debug adapter %q is not available", requested)
	}
	if len(values) == 1 {
		return values[0], nil
	}
	names := make([]string, 0, len(values))
	for _, value := range values {
		names = append(names, value.adapter.Name)
	}
	slices.Sort(names)
	return detectedAdapter{}, fmt.Errorf("multiple debug adapters are available; choose one of: %s", strings.Join(names, ", "))
}

func resolvePlan(workspace string, selected detectedAdapter, options StartOptions) (Plan, error) {
	requestName := strings.ToLower(strings.TrimSpace(options.Request))
	if requestName == "" {
		requestName = "launch"
	}
	if requestName != "launch" && requestName != "attach" {
		return Plan{}, fmt.Errorf("request must be launch or attach (got %q)", options.Request)
	}
	ioMode := options.IO
	if ioMode == "" {
		ioMode = IOOutput
	}
	if ioMode != IOOutput && ioMode != IOTerminal {
		return Plan{}, fmt.Errorf("I/O mode must be %s or %s (got %q)", IOOutput, IOTerminal, options.IO)
	}
	if ioMode == IOTerminal && selected.adapter.TerminalStrategy == TerminalUnsupported {
		return Plan{}, fmt.Errorf("debug adapter %s does not support a terminal", selected.adapter.Name)
	}

	projectDir, err := selectProjectDir(workspace, selected.projects, options.ProjectDir, options.Configuration)
	if err != nil {
		return Plan{}, err
	}
	arguments := maps.Clone(selected.adapter.Defaults)
	if arguments == nil {
		arguments = make(map[string]any)
	}
	maps.Copy(arguments, options.Configuration)
	arguments, err = ResolveConfigurationPaths(workspace, projectDir, selected.adapter.ConfigurationPaths, arguments)
	if err != nil {
		return Plan{}, err
	}
	if _, exists := arguments["name"]; !exists {
		arguments["name"] = "Wingman debug"
	}
	arguments["request"] = requestName
	if selected.adapter.IOConfigKey != "" {
		value := string(ioMode)
		if mapped := selected.adapter.IOValues[ioMode]; mapped != "" {
			value = mapped
		}
		arguments[selected.adapter.IOConfigKey] = value
	}

	targetKey := selected.adapter.TargetConfigKey
	if targetKey == "" {
		targetKey = "program"
	}
	target, _ := arguments[targetKey].(string)
	mode, _ := arguments["mode"].(string)
	return Plan{
		Adapter:    selected.adapter,
		ProjectDir: projectDir,
		Target:     target,
		Mode:       mode,
		Request:    requestName,
		IO:         ioMode,
		Arguments:  arguments,
		PreLaunch:  cloneProcessLaunch(options.PreLaunch),
	}, nil
}

func cloneProcessLaunch(value *ProcessLaunch) *ProcessLaunch {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Args = slices.Clone(value.Args)
	return &clone
}

// ResolveConfigurationPaths validates and resolves the path fields declared by
// an adapter descriptor. All other adapter configuration remains opaque.
func ResolveConfigurationPaths(workspace, projectDir string, fields []ConfigurationPath, configuration map[string]any) (map[string]any, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	if !filepath.IsAbs(projectDir) {
		projectDir = filepath.Join(workspace, projectDir)
	}
	resolved := maps.Clone(configuration)
	if resolved == nil {
		resolved = make(map[string]any)
	}
	for _, field := range fields {
		value, ok := resolved[field.Key].(string)
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			continue
		}
		if strings.Contains(value, "${") {
			return nil, fmt.Errorf("configuration %s must be a concrete workspace path", field.Key)
		}
		path := filepath.FromSlash(value)
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectDir, path)
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve configuration %s: %w", field.Key, err)
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("configuration %s must stay inside the workspace", field.Key)
		}
		info, statErr := os.Stat(path)
		if statErr != nil && (!field.AllowMissing || !errors.Is(statErr, os.ErrNotExist)) {
			return nil, fmt.Errorf("configuration %s path %q does not exist", field.Key, value)
		}
		if err := ensurePotentialPathInside(workspace, path, field.AllowMissing); err != nil {
			return nil, fmt.Errorf("configuration %s path %q: %w", field.Key, value, err)
		}
		if statErr == nil && field.Directory && !info.IsDir() {
			return nil, fmt.Errorf("configuration %s path %q must be a directory", field.Key, value)
		}
		resolved[field.Key] = filepath.Clean(path)
	}
	return resolved, nil
}

func ensurePotentialPathInside(workspace, path string, allowMissing bool) error {
	resolvedWorkspace, err := pathutil.Resolve(workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	resolve := pathutil.Resolve
	if allowMissing {
		resolve = pathutil.ResolveExistingPrefix
	}
	resolvedPath, err := resolve(path)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}
	rel, err := filepath.Rel(resolvedWorkspace, resolvedPath)
	if err != nil || !filepath.IsLocal(rel) {
		return errors.New("must stay inside the workspace after resolving symlinks")
	}
	return nil
}

func selectProjectDir(workspace string, projects []string, requested string, configuration map[string]any) (string, error) {
	if strings.TrimSpace(requested) != "" {
		path, err := ResolveWorkspaceDirectory(workspace, requested)
		if err != nil {
			return "", err
		}
		return path, nil
	}

	for _, key := range []string{"cwd", "dlvCwd", "program"} {
		value, _ := configuration[key].(string)
		if value == "" {
			continue
		}
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspace, path)
		}
		if info, err := os.Stat(path); err == nil {
			if !info.IsDir() {
				path = filepath.Dir(path)
			}
			if project := nearestProject(projects, path); project != "" {
				return project, nil
			}
		}
	}
	if len(projects) == 1 {
		return projects[0], nil
	}
	if len(projects) == 0 {
		return "", errors.New("adapter has no detected project root")
	}
	relative := make([]string, 0, len(projects))
	for _, project := range projects {
		rel, err := filepath.Rel(workspace, project)
		if err != nil {
			rel = project
		}
		relative = append(relative, filepath.ToSlash(rel))
	}
	return "", fmt.Errorf("multiple adapter project roots found; set project_dir to one of: %s", strings.Join(relative, ", "))
}

func ResolveWorkspaceDirectory(workspace, value string) (string, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve project_dir: %w", err)
	}
	rel, err := filepath.Rel(workspace, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("project_dir must stay inside the workspace")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("project_dir %q: %w", value, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project_dir %q is not a directory", value)
	}
	if err := ensurePotentialPathInside(workspace, abs, false); err != nil {
		return "", fmt.Errorf("project_dir %q: %w", value, err)
	}
	return filepath.Clean(abs), nil
}

func nearestProject(projects []string, target string) string {
	best := ""
	for _, project := range projects {
		rel, err := filepath.Rel(project, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(project) > len(best) {
			best = project
		}
	}
	return best
}

func (m *Manager) Session(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("DAP manager is closed")
	}
	if m.session == nil {
		return nil, errors.New("no debug session is active; start one first")
	}
	if id != "" && id != m.session.ID() {
		return nil, fmt.Errorf("debug session %q not found", id)
	}
	return m.session, nil
}

// ActiveSession returns the most recently started session, if any. The
// returned Session owns its own synchronization and remains valid until the
// manager is closed.
func (m *Manager) ActiveSession() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	return m.session
}

// Breakpoints returns the editor-owned breakpoint set. These breakpoints are
// applied automatically to subsequently launched sessions.
func (m *Manager) Breakpoints(path string) []SourceBreakpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.breakpoints[filepath.Clean(path)])
}

// SetBreakpoints replaces one file's editor breakpoints and returns them with
// stable ids. Without a running session they are pending; a starting session
// picks them up during configuration or right after it becomes active.
func (m *Manager) SetBreakpoints(ctx context.Context, path string, values []SourceBreakpoint) ([]Breakpoint, error) {
	if err := validateSourceBreakpoints(values); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	m.breakpointMu.Lock()
	defer m.breakpointMu.Unlock()
	m.mu.Lock()
	m.breakpoints[path] = slices.Clone(values)
	if len(values) == 0 {
		delete(m.breakpoints, path)
	}
	m.breakpointsAt++
	m.breakpointEdits[path] = m.breakpointsAt
	previous := m.breakpointIDs[path]
	ids := make(map[[2]int]int, len(values))
	for _, value := range values {
		position := [2]int{value.Line, value.Column}
		if id, ok := previous[position]; ok {
			ids[position] = id
		} else if _, ok := ids[position]; !ok {
			m.nextBreakpointID++
			ids[position] = m.nextBreakpointID
		}
	}
	for position, id := range previous {
		if _, kept := ids[position]; !kept {
			delete(m.resolved, id)
		}
	}
	if len(ids) == 0 {
		delete(m.breakpointIDs, path)
	} else {
		m.breakpointIDs[path] = ids
	}
	session := m.session
	m.mu.Unlock()
	if session == nil || session.Status().State == StateTerminated {
		return m.publishResolved(path, values, nil), nil
	}
	resolved, err := session.SetBreakpoints(ctx, path, values)
	if err != nil {
		return nil, err
	}
	return m.publishResolved(path, values, resolved), nil
}

// SetExceptionBreakpoints selects exception filters for the current and
// future sessions; until called, sessions use the adapter's defaults.
func (m *Manager) SetExceptionBreakpoints(ctx context.Context, filters []string) error {
	m.mu.Lock()
	m.exceptionFilters, m.exceptionsSet = slices.Clone(filters), true
	session := m.session
	m.mu.Unlock()
	if session == nil || session.Status().State == StateTerminated {
		return nil
	}
	return session.SetExceptionBreakpoints(ctx, filters)
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	return m.Disconnect(ctx, id, true)
}

// Terminate asks the debuggee to exit gracefully, falling back to a
// terminating disconnect when the adapter has no terminate request.
func (m *Manager) Terminate(ctx context.Context, id string) error {
	session, err := m.Session(id)
	if err != nil {
		return err
	}
	if requested, err := session.Terminate(ctx); requested {
		return err
	}
	return m.Disconnect(ctx, id, true)
}

// Disconnect ends the session; terminate also ends the debuggee rather than
// leaving it running detached from the debugger.
func (m *Manager) Disconnect(ctx context.Context, id string, terminate bool) error {
	session, err := m.Session(id)
	if err != nil {
		return err
	}
	status := session.Status()
	disconnectErr := session.Disconnect(ctx, terminate)
	m.mu.Lock()
	if m.session == session {
		m.session = nil
		m.notifyChangedLocked()
	}
	m.mu.Unlock()
	if disconnectErr != nil && status.State != StateTerminated {
		return disconnectErr
	}
	return nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	session := m.session
	m.session = nil
	m.notifyChangedLocked()
	m.mu.Unlock()
	if session != nil {
		session.Close()
	}
}

// Changes wakes workspace observers when a session is created, replaced or removed.
func (m *Manager) Changes() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.changed == nil {
		m.changed = make(chan struct{})
	}
	return m.changed
}
func (m *Manager) notifyChangedLocked() {
	if m.changed != nil {
		close(m.changed)
		m.changed = nil
	}
}
