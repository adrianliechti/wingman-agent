package code

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"
	"go.yaml.in/yaml/v4"

	"github.com/adrianliechti/wingman-agent/pkg/changes"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	lsptool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/lsp"
	mcptool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/plugin"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

//go:embed all:skills
var bundledFS embed.FS

const memoryMaxBytes = 25 * 1024

type UI interface {
	Elicit(ctx context.Context, req tool.ElicitRequest) (tool.ElicitResult, error)
	Confirm(ctx context.Context, message string) (bool, error)
}

// Notice is a live session advisory, displayed separately from model messages.
type Notice struct {
	Severity string
	Message  string
}

func (n Notice) String() string {
	switch n.Severity {
	case "warning":
		return "Warning: " + n.Message
	case "error":
		return "Error: " + n.Message
	default:
		return n.Message
	}
}

type sessionCtxKey struct{}

func WithSessionID(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, sid)
}

func SessionIDFromContext(ctx context.Context) string {
	sid, _ := ctx.Value(sessionCtxKey{}).(string)
	return sid
}

type Workspace struct {
	Root        *os.Root
	RootPath    string
	MemoryPath  string
	ScratchPath string

	Plugins []plugin.Plugin

	MCP *mcp.Manager

	language *language.Service
	Changes  *changes.Manager

	warmupOnce sync.Once

	mu               sync.RWMutex
	closed           bool
	warmed           bool
	mcpToolsByServer map[string][]tool.Tool
	mcpApproval      *tool.Elicitation
	lspTools         []tool.Tool

	mcpCatalogOnce    sync.Once
	mcpCatalogGate    chan struct{}
	mcpTrustDecisions map[string]bool
	hookTrust         *trustFile
	mcpRefreshCancel  context.CancelFunc

	// LSP calls may include server startup and network round-trips. Keep their
	// lifetime lock separate from workspace state so close does not block
	// unrelated workspace readers.
	lspLifeMu sync.RWMutex

	memoryMu          sync.Mutex
	memoryCache       string
	memoryFingerprint string

	skillsMu      sync.RWMutex
	skillsRefresh sync.Mutex
	baseSkills    []skill.Skill
	skills        []skill.Skill
}

func NewWorkspace(workDir string) (*Workspace, error) {
	workDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}

	root, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}

	scratchDir := filepath.Join(os.TempDir(), "wingman-"+uuid.New().String())
	if err := os.MkdirAll(scratchDir, 0755); err != nil {
		root.Close()
		return nil, fmt.Errorf("create scratch directory: %w", err)
	}
	bundled, _, err := loadBundledSkills()
	if err != nil {
		os.RemoveAll(scratchDir)
		root.Close()
		return nil, fmt.Errorf("load bundled skills: %w", err)
	}

	memoryDir := projectMemoryDir(workDir)
	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		os.RemoveAll(scratchDir)
		root.Close()
		return nil, fmt.Errorf("create memory directory: %w", err)
	}

	plugins, diagnostics := plugin.Discover(workDir, projectPluginDataDir(workDir), personalPluginDataDir())
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(os.Stderr, "plugin: %s\n", diagnostic)
	}

	personal := skill.MustDiscoverPersonal()
	discovered := skill.MustDiscover(workDir)
	baseSkills := skill.Merge(bundled, plugin.Skills(plugins))
	mergedSkills := skill.Merge(skill.Merge(baseSkills, personal), discovered)
	reportShadowedPluginSkills(mergedSkills)

	mcpManager := loadMCP(workDir, plugins)

	return &Workspace{
		Root:        root,
		RootPath:    workDir,
		MemoryPath:  memoryDir,
		ScratchPath: scratchDir,
		Plugins:     plugins,
		MCP:         mcpManager,
		baseSkills:  baseSkills,
		skills:      mergedSkills,
	}, nil
}

// Skills returns a stable snapshot of the current skill catalog.
func (w *Workspace) Skills() []skill.Skill {
	w.skillsMu.RLock()
	defer w.skillsMu.RUnlock()
	return cloneSkills(w.skills)
}

func cloneSkills(skills []skill.Skill) []skill.Skill {
	result := slices.Clone(skills)
	for i := range result {
		result[i].Metadata = maps.Clone(result[i].Metadata)
		result[i].AllowedTools = slices.Clone(result[i].AllowedTools)
		result[i].Arguments = slices.Clone(result[i].Arguments)
	}
	return result
}

// RefreshSkills rediscovers personal and project Agent Skills and atomically
// replaces the catalog when its metadata or precedence changed. Plugin skills
// stay fixed because reloading a plugin also affects hooks, MCP, and permissions.
func (w *Workspace) RefreshSkills() bool {
	w.skillsRefresh.Lock()
	defer w.skillsRefresh.Unlock()

	personal, err := skill.RediscoverPersonal()
	if err != nil {
		return false
	}
	project, err := skill.Rediscover(w.RootPath)
	if err != nil {
		return false
	}
	next := skill.Merge(skill.Merge(w.baseSkills, personal), project)

	w.skillsMu.Lock()
	if reflect.DeepEqual(w.skills, next) {
		w.skillsMu.Unlock()
		return false
	}
	w.skills = next
	w.skillsMu.Unlock()

	return true
}

// loadMCP layers the project and global configs over the servers plugins
// contribute, then drops entries that duplicate an endpoint already configured
// so its tools are not offered twice.
func loadMCP(workDir string, plugins []plugin.Plugin) *mcp.Manager {
	servers, diagnostics := plugin.Servers(plugins)
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(os.Stderr, "plugin: %s\n", diagnostic)
	}

	// Load each file on its own so one broken config does not hide the other.
	projectConfig := filepath.Join(workDir, "mcp.json")
	configs := map[string]mcp.ServerConfig{}
	trustRequired := map[string]string{}
	found := false
	for _, path := range []string{globalMCPConfigPath(), projectConfig} {
		if path == "" {
			continue
		}
		cfg, err := mcp.LoadConfig(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp: %s: %v\n", path, err)
			continue
		}
		found = true
		maps.Copy(configs, cfg.Servers)
		if path == projectConfig {
			for name := range cfg.Servers {
				trustRequired[name] = projectConfig
			}
		}
	}

	if !found && len(servers) == 0 {
		return nil
	}

	manager := mcp.NewManager(&mcp.Config{Servers: configs})
	manager.TrustRequired = trustRequired
	configured := slices.Sorted(maps.Keys(manager.Servers))

	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if _, ok := manager.Servers[name]; ok {
			fmt.Fprintf(os.Stderr, "plugin: MCP server %q is shadowed by your own configuration\n", name)
			continue
		}

		manager.Servers[name] = servers[name]
		manager.TrustRequired[name] = "plugin configuration at " + servers[name].Dir
	}

	for _, note := range mcp.Dedup(manager.Servers, configured) {
		fmt.Fprintf(os.Stderr, "mcp: %s\n", note)
	}

	manager.Dir = workDir
	return manager
}

func reportShadowedPluginSkills(skills []skill.Skill) {
	for i := range skills {
		if skills[i].Plugin == "" {
			continue
		}

		if winner := skill.FindSkill(skills[i].Name, skills); winner != &skills[i] {
			fmt.Fprintf(os.Stderr, "plugin: skill %q is shadowed; invoke it as /%s\n", skills[i].Name, skills[i].Qualified())
		}
	}
}

// WarmUp starts no language server: each starts when a file it handles is first queried or edited.
func (w *Workspace) WarmUp() {
	w.warmupOnce.Do(func() {
		var changesManager *changes.Manager
		if isGitRepo(w.RootPath) {
			changesManager = changes.New(w.RootPath)
		}

		servers, diagnostics := plugin.LanguageServers(w.Plugins, w.RootPath)
		for _, diagnostic := range diagnostics {
			fmt.Fprintf(os.Stderr, "plugin: %s\n", diagnostic)
		}
		languageService := language.New(w.RootPath, servers...)
		lspTools := w.protectLSPTools(lsptool.NewTools(languageService))

		w.lspLifeMu.Lock()
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			w.lspLifeMu.Unlock()
			languageService.Close()
			if changesManager != nil {
				changesManager.Close()
			}
			return
		}
		w.Changes = changesManager
		w.language = languageService
		w.lspTools = lspTools
		w.warmed = true
		w.mu.Unlock()
		w.lspLifeMu.Unlock()
	})
}

// SetMCPApproval must run before InitMCP: tools capture the approver when listed.
func (w *Workspace) SetMCPApproval(elicit *tool.Elicitation) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mcpApproval = elicit
}

func (w *Workspace) InitMCP(ctx context.Context, ui ...UI) error {
	w.mu.RLock()
	manager := w.MCP
	closed := w.closed
	approval := w.mcpApproval
	w.mu.RUnlock()
	if manager == nil || closed {
		return nil
	}

	if err := w.lockMCPCatalog(ctx); err != nil {
		return err
	}
	defer w.unlockMCPCatalog()
	w.startMCPToolRefreshes(manager)
	var confirm UI
	if len(ui) > 0 {
		confirm = ui[0]
	}
	connectErr := manager.Connect(ctx, func(ctx context.Context, name string, server mcp.ServerConfig) error {
		return w.approveMCP(ctx, manager, confirm, name, server)
	})
	w.mu.RLock()
	closed = w.closed
	w.mu.RUnlock()
	if closed {
		// Close already closed the sessions it saw; Connect may have added more.
		manager.Close()
		return connectErr
	}

	sessions := manager.Sessions()
	toolsByServer := make(map[string][]tool.Tool)
	toolsErrs := []error{connectErr}
	for _, serverName := range slices.Sorted(maps.Keys(sessions)) {
		session := sessions[serverName]
		tools, err := mcptool.ToolsForServer(ctx, serverName, session, approval)
		if err != nil {
			toolsErrs = append(toolsErrs, mcp.Redact(fmt.Errorf("list MCP tools from %s: %w", serverName, err)))
			continue
		}
		toolsByServer[serverName] = tools
	}

	w.mu.Lock()
	if !w.closed {
		w.mcpToolsByServer = toolsByServer
	}
	w.mu.Unlock()
	return errors.Join(toolsErrs...)
}

// MCP initialization can wait for another session's approval dialog. Waiting
// callers must remain cancellable without cancelling the dialog's owner.
func (w *Workspace) lockMCPCatalog(ctx context.Context) error {
	w.mcpCatalogOnce.Do(func() { w.mcpCatalogGate = make(chan struct{}, 1) })
	select {
	case w.mcpCatalogGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			w.unlockMCPCatalog()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Workspace) unlockMCPCatalog() { <-w.mcpCatalogGate }

func (w *Workspace) startMCPToolRefreshes(manager *mcp.Manager) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	if w.mcpRefreshCancel != nil {
		w.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.mcpRefreshCancel = cancel
	manager.SetToolListChangedHandler(func(serverName string) {
		w.scheduleMCPToolRefresh(ctx, manager, serverName)
	})
	w.mu.Unlock()
}

func (w *Workspace) scheduleMCPToolRefresh(ctx context.Context, manager *mcp.Manager, serverName string) {
	if ctx == nil {
		return
	}
	go w.refreshMCPServerTools(ctx, manager, serverName)
}

func (w *Workspace) refreshMCPServerTools(ctx context.Context, manager *mcp.Manager, serverName string) {
	if err := w.lockMCPCatalog(ctx); err != nil {
		return
	}
	defer w.unlockMCPCatalog()

	session := manager.Session(serverName)
	if session == nil {
		return
	}
	w.mu.RLock()
	approval := w.mcpApproval
	w.mu.RUnlock()

	tools, err := mcptool.ToolsForServer(ctx, serverName, session, approval)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "warning: failed to refresh tools from MCP server %s: %v\n", serverName, err)
		}
		return
	}
	if manager.Session(serverName) != session {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if w.mcpToolsByServer == nil {
		w.mcpToolsByServer = make(map[string][]tool.Tool)
	}
	w.mcpToolsByServer[serverName] = tools
}

func flattenMCPTools(byServer map[string][]tool.Tool) []tool.Tool {
	var tools []tool.Tool
	for _, serverName := range slices.Sorted(maps.Keys(byServer)) {
		tools = append(tools, byServer[serverName]...)
	}
	return tools
}

func (w *Workspace) Close() {
	w.lspLifeMu.Lock()
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.lspLifeMu.Unlock()
		return
	}
	w.closed = true
	mcpManager := w.MCP
	languageService := w.language
	changesManager := w.Changes
	root := w.Root
	scratchPath := w.ScratchPath
	mcpRefreshCancel := w.mcpRefreshCancel
	w.MCP = nil
	w.language = nil
	w.Changes = nil
	w.mcpToolsByServer = nil
	w.mcpRefreshCancel = nil
	w.lspTools = nil
	w.mu.Unlock()
	if mcpManager != nil {
		mcpManager.SetToolListChangedHandler(nil)
	}
	if mcpRefreshCancel != nil {
		mcpRefreshCancel()
	}
	if languageService != nil {
		languageService.Close()
	}
	w.lspLifeMu.Unlock()

	if mcpManager != nil {
		mcpManager.Close()
	}
	if changesManager != nil {
		changesManager.Close()
	}
	if scratchPath != "" {
		os.RemoveAll(scratchPath)
	}
	if root != nil {
		root.Close()
	}
}

func (w *Workspace) IsGitRepo() bool { return isGitRepo(w.RootPath) }

func (w *Workspace) SyncProjectMode() {
	w.mu.RLock()
	available := !w.closed && w.warmed
	w.mu.RUnlock()
	if !available {
		return
	}

	var nextChanges *changes.Manager
	if isGitRepo(w.RootPath) {
		nextChanges = changes.New(w.RootPath)
	}

	w.mu.Lock()
	if w.closed || !w.warmed {
		w.mu.Unlock()
		if nextChanges != nil {
			nextChanges.Close()
		}
		return
	}
	previousChanges := w.Changes
	w.Changes = nextChanges
	w.mu.Unlock()
	if previousChanges != nil {
		previousChanges.Close()
	}
}

// LanguageService returns the service published by WarmUp, or nil after Close.
func (w *Workspace) LanguageService() *language.Service {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.language
}

const maxEditDiagnosticFiles = 8

// WithLanguageSync keeps language servers current with the files tools change and adds their diagnostics to edit results.
func (w *Workspace) WithLanguageSync(tools []tool.Tool) []tool.Tool {
	wrapped := slices.Clone(tools)
	for i := range wrapped {
		name, execute := wrapped[i].Name, wrapped[i].Execute
		if execute == nil {
			continue
		}
		wrapped[i].Execute = func(ctx context.Context, args map[string]any) (tool.Result, error) {
			out, err := execute(ctx, args)
			changes, _ := tool.ResultFileChanges(out.Metadata)
			if len(changes) == 0 {
				return out, err
			}
			service := w.LanguageService()
			if service == nil {
				return out, err
			}
			if service.Running() {
				service.FilesChanged(ctx, w.languageFileChanges(changes))
			}
			if err == nil && name == "edit" {
				if note := w.editDiagnostics(ctx, service, changes); note != "" {
					out.Content.AppendText("\n\n" + note)
				}
			}
			return out, err
		}
	}
	return wrapped
}

// Each file may wait for its server, so the files of a batched edit are checked together.
func (w *Workspace) editDiagnostics(ctx context.Context, service *language.Service, changes []tool.FileChange) string {
	files, _ := tool.NetFileChanges(changes)
	files = slices.DeleteFunc(files, func(change tool.FileChange) bool {
		return !change.AfterExists || change.AfterRef != nil
	})
	files = files[:min(len(files), maxEditDiagnosticFiles)]
	notes := make([]string, len(files))
	var wg sync.WaitGroup
	for i, change := range files {
		path := change.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(w.RootPath, path)
		}
		wg.Go(func() { notes[i] = service.EditDiagnostics(ctx, path, change.After) })
	}
	wg.Wait()
	return strings.Join(slices.DeleteFunc(notes, func(note string) bool { return note == "" }), "\n\n")
}

func (w *Workspace) languageFileChanges(changes []tool.FileChange) []lsp.FileChange {
	absolute := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(w.RootPath, path)
	}
	var events []lsp.FileChange
	for _, change := range changes {
		if change.MovedFrom != "" {
			events = append(events, lsp.FileChange{Path: absolute(change.MovedFrom), Type: lsp.FileChangeTypeDeleted})
		}
		kind := lsp.FileChangeTypeChanged
		switch {
		case !change.AfterExists:
			kind = lsp.FileChangeTypeDeleted
		case !change.BeforeExists || change.MovedFrom != "":
			kind = lsp.FileChangeTypeCreated
		}
		events = append(events, lsp.FileChange{Path: absolute(change.Path), Type: kind})
	}
	return events
}

func (w *Workspace) protectLSPTools(tools []tool.Tool) []tool.Tool {
	protected := append([]tool.Tool(nil), tools...)
	for i := range protected {
		execute := protected[i].Execute
		if execute == nil {
			continue
		}
		protected[i].Execute = func(ctx context.Context, args map[string]any) (tool.Result, error) {
			w.lspLifeMu.RLock()
			defer w.lspLifeMu.RUnlock()
			return execute(ctx, args)
		}
	}
	return protected
}

func (w *Workspace) Diffs(ctx context.Context) ([]changes.FileDiff, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return nil, nil
	}
	return w.Changes.Diffs(ctx)
}

// DiffsIfChanged checks for changes and loads their diffs in one Git scan.
func (w *Workspace) DiffsIfChanged(ctx context.Context, previous uint64) ([]changes.FileDiff, uint64, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return nil, 0, nil
	}
	return w.Changes.DiffsIfChanged(ctx, previous)
}

func (w *Workspace) DiffsLayer(ctx context.Context, layer changes.DiffLayer) ([]changes.FileDiff, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return nil, changes.ErrNotGitRepository
	}
	return w.Changes.DiffsLayer(ctx, layer)
}

func (w *Workspace) DiffsLayerIfChanged(ctx context.Context, layer changes.DiffLayer, previous uint64) ([]changes.FileDiff, uint64, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return nil, 0, changes.ErrNotGitRepository
	}
	return w.Changes.DiffsLayerIfChanged(ctx, layer, previous)
}

func (w *Workspace) Diff(ctx context.Context, path string, layer changes.DiffLayer) (changes.FileDiff, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return changes.FileDiff{}, errors.New("change tracking is not available")
	}
	return w.Changes.Diff(ctx, path, layer)
}

func (w *Workspace) RevertChange(ctx context.Context, path string) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return errors.New("change tracking is not available")
	}
	return w.Changes.Revert(ctx, path)
}

func (w *Workspace) GitInit() error {
	if isGitRepo(w.RootPath) {
		return errors.New("workspace is already a Git repository")
	}
	if err := removeDanglingGitPointer(w.RootPath); err != nil {
		return err
	}
	if _, err := git.PlainInitWithOptions(w.RootPath, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	}); err != nil {
		return fmt.Errorf("initialize Git repository: %w", err)
	}
	w.SyncProjectMode()
	return nil
}

func removeDanglingGitPointer(dir string) error {
	path := filepath.Join(dir, git.GitDirName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect .git: %w", err)
	}
	if info.IsDir() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("workspace contains an unusable .git file: %w", err)
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return errors.New("workspace contains a .git file that is not a Git repository; remove it and try again")
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	if _, err := os.Stat(target); err == nil {
		return errors.New("workspace .git file points to an existing external Git directory; remove it manually to reinitialize")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale .git file: %w", err)
	}
	return nil
}

func (w *Workspace) GitStatus(ctx context.Context) (changes.GitStatus, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return changes.GitStatus{}, changes.ErrNotGitRepository
	}
	return w.Changes.GitStatus(ctx)
}

func (w *Workspace) GitBranches(ctx context.Context, refresh bool) ([]changes.GitBranch, string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return nil, "", changes.ErrNotGitRepository
	}
	return w.Changes.Branches(ctx, refresh)
}

func (w *Workspace) GitHistory(ctx context.Context) ([]changes.GitCommit, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return nil, changes.ErrNotGitRepository
	}
	return w.Changes.History(ctx)
}

func (w *Workspace) GitInsights(ctx context.Context) (changes.GitInsightsResult, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return changes.GitInsightsResult{}, changes.ErrNotGitRepository
	}
	return w.Changes.Insights(ctx)
}

func (w *Workspace) GitHistoryLimit(ctx context.Context, limit int) ([]changes.GitCommit, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return nil, changes.ErrNotGitRepository
	}
	return w.Changes.HistoryLimit(ctx, limit)
}

func (w *Workspace) GitCompare(ctx context.Context, base, head string, mergeBase bool) (changes.CompareResult, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return changes.CompareResult{}, changes.ErrNotGitRepository
	}
	return w.Changes.Compare(ctx, base, head, mergeBase)
}

func (w *Workspace) GitCreateBranch(ctx context.Context, name string) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return changes.ErrNotGitRepository
	}
	return w.Changes.CreateBranch(ctx, name)
}

func (w *Workspace) GitCheckoutBranch(ctx context.Context, name, remote string) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return changes.ErrNotGitRepository
	}
	return w.Changes.CheckoutBranch(ctx, name, remote)
}

func (w *Workspace) GitStage(ctx context.Context, paths []string) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return changes.ErrNotGitRepository
	}
	return w.Changes.Stage(ctx, paths)
}

func (w *Workspace) GitUnstage(ctx context.Context, paths []string) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return changes.ErrNotGitRepository
	}
	return w.Changes.Unstage(ctx, paths)
}

func (w *Workspace) GitCommit(ctx context.Context, message string) (string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return "", changes.ErrNotGitRepository
	}
	return w.Changes.Commit(ctx, message)
}

func (w *Workspace) GitPull(ctx context.Context) (string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return "", changes.ErrNotGitRepository
	}
	return w.Changes.Pull(ctx)
}

func (w *Workspace) GitPush(ctx context.Context) (string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return "", changes.ErrNotGitRepository
	}
	return w.Changes.Push(ctx)
}

func (w *Workspace) MemoryContent() string {
	w.memoryMu.Lock()
	defer w.memoryMu.Unlock()

	files := listMemoryFiles(w.MemoryPath)

	var fp strings.Builder
	for _, f := range files {
		fmt.Fprintf(&fp, "%s\x00%d\n", f.name, f.mtime.UnixNano())
	}
	if fp.String() == w.memoryFingerprint {
		return w.memoryCache
	}

	var lines []string
	for _, f := range files {
		line := fmt.Sprintf("- [%s](%s)", memoryTitle(f.name), f.name)
		if hook := extractMemoryHook(filepath.Join(w.MemoryPath, f.name)); hook != "" {
			line += " — " + hook
		}
		lines = append(lines, line)
	}

	content := strings.Join(lines, "\n")
	if len(content) > memoryMaxBytes {
		content = text.HeadBytes(content, memoryMaxBytes)
		if idx := strings.LastIndex(content, "\n"); idx > 0 {
			content = content[:idx]
		}
		content += "\n\n> WARNING: memory index exceeded 25KB and was truncated."
	}

	w.memoryCache = content
	w.memoryFingerprint = fp.String()
	return content
}

type memoryFile struct {
	name  string
	mtime time.Time
}

func listMemoryFiles(dir string) []memoryFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var files []memoryFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, memoryFile{name: e.Name(), mtime: info.ModTime()})
	}

	slices.SortFunc(files, func(a, b memoryFile) int {
		if c := b.mtime.Compare(a.mtime); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	return files
}

func memoryTitle(name string) string {
	title := strings.TrimSuffix(name, filepath.Ext(name))
	title = strings.NewReplacer("_", " ", "-", " ").Replace(title)
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return name
	}
	runes := []rune(title)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func extractMemoryHook(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(data)

	if fmBody, body, ok := splitFrontmatter(text); ok {
		var fm struct {
			Description string `yaml:"description"`
		}
		if err := yaml.Load([]byte(fmBody), &fm); err == nil {
			if d, _, _ := strings.Cut(strings.TrimSpace(fm.Description), "\n"); d != "" {
				return strings.TrimSpace(d)
			}
		}
		text = body
	}

	for line := range strings.SplitSeq(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		}
	}
	return ""
}

func splitFrontmatter(text string) (fm, body string, ok bool) {
	rest, found := strings.CutPrefix(text, "---\n")
	if !found {
		rest, found = strings.CutPrefix(text, "---\r\n")
	}
	if !found {
		return "", "", false
	}
	// The leading newline lets an empty block close on its first line.
	before, after, ok := strings.Cut("\n"+rest, "\n---")
	if !ok {
		return "", "", false
	}
	body = strings.TrimLeft(after, "\r\n")
	return strings.TrimPrefix(before, "\n"), body, true
}

func (w *Workspace) ManagedTools() (mcpTools, lspTools []tool.Tool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	mcpTools = flattenMCPTools(w.mcpToolsByServer)
	lspTools = append([]tool.Tool(nil), w.lspTools...)
	return
}

func (w *Workspace) HasChanges() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.Changes != nil
}

func (w *Workspace) ChangesFingerprint(ctx context.Context) uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Changes == nil {
		return 0
	}
	return w.Changes.Fingerprint(ctx)
}

func isGitRepo(dir string) bool {
	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{
		DetectDotGit:          true,
		EnableDotGitCommonDir: true,
	})
	if err != nil {
		return false
	}
	_, err = repo.Worktree()
	return err == nil
}

// SessionStorageRoot is the directory used to locate builtin session journals.
// Linked worktrees and subdirectories share their repository's journals.
func SessionStorageRoot(workingDir string) string {
	if root := findCanonicalGitRoot(workingDir); root != "" {
		return root
	}
	return workingDir
}

func projectKey(workingDir string) string {
	root := SessionStorageRoot(workingDir)
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
	}

	sanitized := filepath.Clean(root)

	if vol := filepath.VolumeName(sanitized); vol != "" {
		sanitized = strings.TrimPrefix(sanitized, vol)
	}

	sanitized = strings.TrimPrefix(sanitized, string(filepath.Separator))
	sanitized = strings.ReplaceAll(sanitized, string(filepath.Separator), "_")
	sanitized = strings.ToLower(sanitized)

	return sanitized
}

func globalMCPConfigPath() string {
	return mcp.GlobalConfigPath()
}

// ProjectStateDir holds what Wingman keeps of a folder: its memory, sessions,
// trust decisions and plugin data.
func ProjectStateDir(workingDir string) string {
	path, err := layout.WingmanPath("projects", projectKey(workingDir))
	if err == nil {
		return path
	}

	return filepath.Join(os.TempDir(), ".wingman", "projects", projectKey(workingDir))
}

func projectMemoryDir(workingDir string) string {
	return filepath.Join(ProjectStateDir(workingDir), "memory")
}

// projectPluginDataDir holds PLUGIN_DATA for plugins installed in the project,
// alongside that project's other state so it survives plugin updates.
func projectPluginDataDir(workingDir string) string {
	return filepath.Join(ProjectStateDir(workingDir), "plugin-data")
}

func personalPluginDataDir() string {
	path, _ := layout.WingmanPath("plugin-data")
	return path
}

func SessionsDir(workingDir string) string {
	return filepath.Join(ProjectStateDir(workingDir), "sessions")
}

// SessionStores lists the sessions directory of every project that has state.
func SessionStores() ([]string, error) {
	projects, err := layout.WingmanPath("projects")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(projects)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var stores []string
	for _, entry := range entries {
		if entry.IsDir() {
			stores = append(stores, filepath.Join(projects, entry.Name(), "sessions"))
		}
	}
	return stores, nil
}

func findCanonicalGitRoot(dir string) string {
	cur := filepath.Clean(dir)
	for {
		gitPath := filepath.Join(cur, ".git")
		info, err := os.Lstat(gitPath)
		if err == nil {
			if info.IsDir() {
				return cur
			}
			if root := resolveWorktreeRoot(cur, gitPath); root != "" {
				return root
			}
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func resolveWorktreeRoot(worktreeDir, gitFile string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}

	const prefix = "gitdir:"
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, prefix) {
		return ""
	}

	gitdir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktreeDir, gitdir)
	}
	gitdir = filepath.Clean(gitdir)

	if data, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		common := strings.TrimSpace(string(data))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
		return filepath.Dir(filepath.Clean(common))
	}

	// Submodules also live under .git (in modules/), but they are their own project.
	parent := filepath.Dir(filepath.Dir(gitdir))
	if filepath.Base(filepath.Dir(gitdir)) == "worktrees" && filepath.Base(parent) == ".git" {
		return filepath.Dir(parent)
	}
	return ""
}

var bundledSkillsInstallMu sync.Mutex

const bundledSkillsMarkerFile = ".wingman"

func loadBundledSkills() ([]skill.Skill, string, error) {
	source, err := fs.Sub(bundledFS, "skills")
	if err != nil {
		return nil, "", err
	}

	destination, err := skill.SystemSkillsRoot()
	if err != nil {
		return nil, "", err
	}
	fingerprint, err := bundledSkillsFingerprint(source)
	if err != nil {
		return nil, "", fmt.Errorf("fingerprint system skills: %w", err)
	}

	// .system is a reserved Wingman-owned snapshot. Refresh it wholesale so
	// changed files are replaced and skills removed from the embedded bundle do
	// not survive as stale personal-looking entries. Serialize workspace starts
	// within this process so two projects cannot interleave the refresh.
	bundledSkillsInstallMu.Lock()
	defer bundledSkillsInstallMu.Unlock()
	if !bundledSkillsInstalled(destination, fingerprint) {
		if err := installBundledSkills(source, destination, fingerprint); err != nil {
			return nil, "", err
		}
	}

	skills, err := skill.LoadBundledAt(source, ".", destination)
	return skills, destination, err
}

func bundledSkillsInstalled(destination, fingerprint string) bool {
	marker, err := os.ReadFile(filepath.Join(destination, bundledSkillsMarkerFile))
	return err == nil && strings.TrimSpace(string(marker)) == fingerprint
}

// Staging outside skill roots and swapping by rename keeps other processes from seeing a partial tree.
func installBundledSkills(source fs.FS, destination, fingerprint string) error {
	stagingRoot := filepath.Dir(filepath.Dir(destination))
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return fmt.Errorf("create system skills: %w", err)
	}
	staging, err := os.MkdirTemp(stagingRoot, ".skills-system-*")
	if err != nil {
		return fmt.Errorf("create system skills: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := os.CopyFS(staging, source); err != nil {
		return fmt.Errorf("copy system skills: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, bundledSkillsMarkerFile), []byte(fingerprint+"\n"), 0644); err != nil {
		return fmt.Errorf("mark system skills: %w", err)
	}

	previous := staging + "-previous"
	defer os.RemoveAll(previous)
	var swapErr error
	for range 5 {
		if swapErr = os.Rename(staging, destination); swapErr == nil {
			return nil
		}
		if bundledSkillsInstalled(destination, fingerprint) {
			return nil
		}
		os.RemoveAll(previous)
		if err := os.Rename(destination, previous); err != nil && !errors.Is(err, fs.ErrNotExist) {
			swapErr = err
		}
	}
	return fmt.Errorf("install system skills: %w", swapErr)
}

func bundledSkillsFingerprint(source fs.FS) (string, error) {
	digest := sha256.New()
	writeField := func(value []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write(value)
	}

	err := fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			writeField([]byte("directory"))
			writeField([]byte(name))
			return nil
		}

		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		writeField([]byte("file"))
		writeField([]byte(name))
		writeField(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

// CallMCPTool runs a tool on an already-connected MCP server for mcp_tool hooks.
func (w *Workspace) CallMCPTool(ctx context.Context, server, name string, input map[string]any) (string, error) {
	w.mu.RLock()
	manager := w.MCP
	w.mu.RUnlock()
	if manager == nil {
		return "", errors.New("MCP is not available in this workspace")
	}
	session := manager.Session(server)
	if session == nil {
		return "", fmt.Errorf("MCP server %q is not connected", server)
	}
	return mcptool.CallText(ctx, session, name, input)
}
