package code

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/google/uuid"
	"go.yaml.in/yaml/v4"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	graphtool "github.com/adrianliechti/wingman-agent/pkg/agent/tool/graph"
	lsptool "github.com/adrianliechti/wingman-agent/pkg/agent/tool/lsp"
	toolmcp "github.com/adrianliechti/wingman-agent/pkg/agent/tool/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/graph"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/rewind"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
)

//go:embed skills/*/SKILL.md
var bundledFS embed.FS

const memoryMaxBytes = 25 * 1024

type UI interface {
	Elicit(ctx context.Context, req tool.ElicitRequest) (tool.ElicitResult, error)
	Confirm(ctx context.Context, message string) (bool, error)
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

	Skills []skill.Skill

	MCP *mcp.Manager

	LSP    *lsp.Manager
	Rewind *rewind.Manager
	Graph  *graph.Engine

	warmupOnce sync.Once

	mu         sync.Mutex
	mcpTools   []tool.Tool
	lspTools   []tool.Tool
	graphTools []tool.Tool

	memoryMu          sync.Mutex
	memoryCache       string
	memoryFingerprint string
}

func NewWorkspace(workDir string) (*Workspace, error) {
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}

	scratchDir := filepath.Join(os.TempDir(), "wingman-"+uuid.New().String())
	if err := os.MkdirAll(scratchDir, 0755); err != nil {
		root.Close()
		return nil, fmt.Errorf("create scratch directory: %w", err)
	}

	memoryDir := projectMemoryDir(workDir)
	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		os.RemoveAll(scratchDir)
		root.Close()
		return nil, fmt.Errorf("create memory directory: %w", err)
	}

	bundled := loadBundledSkills()
	personal := skill.MustDiscoverPersonal()
	discovered := skill.MustDiscover(workDir)
	mergedSkills := skill.Merge(skill.Merge(bundled, personal), discovered)

	mcpManager, _ := mcp.Load(globalMCPConfigPath(), filepath.Join(workDir, "mcp.json"))
	if mcpManager != nil {
		mcpManager.Dir = workDir
	}

	return &Workspace{
		Root:        root,
		RootPath:    workDir,
		MemoryPath:  memoryDir,
		ScratchPath: scratchDir,
		Skills:      mergedSkills,
		MCP:         mcpManager,
	}, nil
}

func (w *Workspace) WarmUp() {
	w.warmupOnce.Do(func() {
		if !isSupportedWorkspace(w.RootPath) {
			return
		}

		rewind.CleanupOrphans()
		rewindManager := rewind.New(w.RootPath)

		var lspManager *lsp.Manager
		var lspTools []tool.Tool
		if isGitRepo(w.RootPath) {
			lspManager = lsp.NewManager(w.RootPath)
			lspTools = lsptool.NewTools(lspManager)
		}

		graphEngine := graph.New(w.RootPath, filepath.Join(projectGraphDir(w.RootPath), "graph.json"), graph.WithResolver(&lspResolver{ws: w}))
		graphTools := graphtool.NewTools(graphEngine)

		w.mu.Lock()
		w.Rewind = rewindManager
		w.LSP = lspManager
		w.lspTools = lspTools
		w.Graph = graphEngine
		w.graphTools = graphTools
		w.mu.Unlock()
	})
}

func (w *Workspace) InitMCP(ctx context.Context) error {
	if w.MCP == nil {
		return nil
	}

	if err := w.MCP.Connect(ctx); err != nil {
		return err
	}

	mcpTools, err := toolmcp.Tools(ctx, w.MCP)
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.mcpTools = mcpTools
	w.mu.Unlock()

	return nil
}

func (w *Workspace) Close() {
	if w.MCP != nil {
		w.MCP.Close()
	}
	if w.LSP != nil {
		w.LSP.Close()
	}
	if w.Rewind != nil {
		w.Rewind.Cleanup()
	}
	if w.ScratchPath != "" {
		os.RemoveAll(w.ScratchPath)
	}
	if w.Root != nil {
		w.Root.Close()
	}
}

func (w *Workspace) IsGitRepo() bool { return isGitRepo(w.RootPath) }

func (w *Workspace) SyncProjectMode() {
	if w.Rewind == nil {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	oldLSP := w.LSP
	if isGitRepo(w.RootPath) {
		w.LSP = lsp.NewManager(w.RootPath)
		w.lspTools = lsptool.NewTools(w.LSP)
	} else {
		w.LSP = nil
		w.lspTools = nil
	}

	if oldLSP != nil {
		oldLSP.Close()
	}
}

func (w *Workspace) Diagnostics(ctx context.Context) map[string][]lsp.Diagnostic {
	if w.LSP == nil {
		return nil
	}
	return w.LSP.CollectAllDiagnostics(ctx)
}

func (w *Workspace) Commit(msg string) error {
	if w.Rewind == nil {
		return nil
	}
	return w.Rewind.Commit(msg)
}

func (w *Workspace) Diffs() ([]rewind.FileDiff, error) {
	if w.Rewind == nil {
		return nil, nil
	}
	return w.Rewind.DiffFromBaseline()
}

func (w *Workspace) Checkpoints() ([]rewind.Checkpoint, error) {
	if w.Rewind == nil {
		return nil, nil
	}
	return w.Rewind.List()
}

func (w *Workspace) Restore(hash string) error {
	if w.Rewind == nil {
		return errors.New("checkpoint tracking is not available for this workspace")
	}
	return w.Rewind.Restore(hash)
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
		line := "- " + f.name
		if hook := extractMemoryHook(filepath.Join(w.MemoryPath, f.name)); hook != "" {
			line += " — " + hook
		}
		lines = append(lines, line)
	}

	content := strings.Join(lines, "\n")
	if len(content) > memoryMaxBytes {
		content = content[:memoryMaxBytes]
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

	slices.SortFunc(files, func(a, b memoryFile) int { return strings.Compare(a.name, b.name) })
	return files
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
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", false
	}
	body = strings.TrimLeft(rest[end+len("\n---"):], "\r\n")
	return rest[:end], body, true
}

func (w *Workspace) ManagedTools() (mcpTools, lspTools, graphTools []tool.Tool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	mcpTools = append([]tool.Tool(nil), w.mcpTools...)
	lspTools = append([]tool.Tool(nil), w.lspTools...)
	graphTools = append([]tool.Tool(nil), w.graphTools...)
	return
}

func isGitRepo(dir string) bool {
	_, err := git.PlainOpen(dir)
	return err == nil
}

var (
	workspaceMaxEntries = 200_000
	workspaceMaxBytes   = int64(2 << 30)
	workspaceWalkBudget = 4 * time.Second
)

func isSupportedWorkspace(dir string) bool {
	if isGitRepo(dir) {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), workspaceWalkBudget)
	defer cancel()

	tooLarge := errors.New("workspace too large")

	result := make(chan bool, 1)
	go func() {
		var entries int
		var size int64

		err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil
			}

			entries++
			if entries > workspaceMaxEntries {
				return tooLarge
			}

			if !d.IsDir() {
				if info, err := d.Info(); err == nil {
					size += info.Size()
					if size > workspaceMaxBytes {
						return tooLarge
					}
				}
			}

			return nil
		})
		result <- err == nil
	}()

	select {
	case ok := <-result:
		return ok
	case <-ctx.Done():
		return false
	}
}

func projectKey(workingDir string) string {
	root := findCanonicalGitRoot(workingDir)
	if root == "" {
		root = workingDir
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
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".wingman", "mcp.json")
}

func projectMemoryDir(workingDir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}

	return filepath.Join(home, ".wingman", "projects", projectKey(workingDir), "memory")
}

func projectGraphDir(workingDir string) string {
	return filepath.Join(filepath.Dir(projectMemoryDir(workingDir)), "graph")
}

func SessionsDir(workingDir string) string {
	return filepath.Join(filepath.Dir(projectMemoryDir(workingDir)), "sessions")
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
			return resolveWorktreeRoot(cur, gitPath)
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

	parent := filepath.Dir(filepath.Dir(gitdir))
	if filepath.Base(parent) == ".git" {
		return filepath.Dir(parent)
	}
	return ""
}

func loadBundledSkills() []skill.Skill {
	skills, _ := skill.LoadBundled(bundledFS, "skills")
	return skills
}
