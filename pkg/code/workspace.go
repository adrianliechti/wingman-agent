package code

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	lsptool "github.com/adrianliechti/wingman-agent/pkg/agent/tool/lsp"
	toolmcp "github.com/adrianliechti/wingman-agent/pkg/agent/tool/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/rewind"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
)

//go:embed skills/*/SKILL.md
var bundledFS embed.FS

// UI is the elicitation hook a frontend provides for tool ask/confirm
// prompts. Pass nil to NewAgent for safe defaults (Confirm → true, Ask → "").
type UI interface {
	Ask(ctx context.Context, message string) (string, error)
	Confirm(ctx context.Context, message string) (bool, error)
}

type Workspace struct {
	Root        *os.Root
	RootPath    string
	MemoryPath  string
	ScratchPath string

	Skills []skill.Skill

	MCP *mcp.Manager
	// LSP and Rewind are set by WarmUp; nil for unsupported workspaces.
	LSP    *lsp.Manager
	Rewind *rewind.Manager

	warmupOnce sync.Once

	mu       sync.Mutex
	mcpTools []tool.Tool
	lspTools []tool.Tool
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

	// Skill precedence (later overrides earlier): bundled → personal → project.
	bundled := loadBundledSkills()
	personal := skill.MustDiscoverPersonal()
	discovered := skill.MustDiscover(workDir)
	mergedSkills := skill.Merge(skill.Merge(bundled, personal), discovered)

	mcpManager, _ := mcp.Load(filepath.Join(workDir, "mcp.json"))

	return &Workspace{
		Root:        root,
		RootPath:    workDir,
		MemoryPath:  memoryDir,
		ScratchPath: scratchDir,
		Skills:      mergedSkills,
		MCP:         mcpManager,
	}, nil
}

// WarmUp probes the workspace and initializes Rewind/LSP. Idempotent.
// Resulting modes:
//   - supported git repo  → Rewind set, LSP set, lspTools set
//   - supported scratch   → Rewind set, LSP nil, lspTools nil
//   - unsupported (huge)  → Rewind nil, LSP nil
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

		w.mu.Lock()
		w.Rewind = rewindManager
		w.LSP = lspManager
		w.lspTools = lspTools
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

// IsGitRepo is re-evaluated each call so callers can react to mid-session
// `git init` / `rm -rf .git`.
func (w *Workspace) IsGitRepo() bool { return isGitRepo(w.RootPath) }

// SyncProjectMode rebuilds LSP when the working dir's git status flips.
// No-op on unsupported workspaces.
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

// Checkpoint operations no-op when Rewind isn't running so call sites
// don't need to nil-check. Use w.Rewind directly for non-user-facing
// poll-loop primitives like Fingerprint.

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
	data, err := os.ReadFile(filepath.Join(w.MemoryPath, memoryFileName))
	if err != nil {
		return ""
	}

	content := strings.TrimSpace(string(data))
	if content == "" {
		return ""
	}

	if len(content) > memoryMaxBytes {
		truncated := content[:memoryMaxBytes]
		if idx := strings.LastIndex(truncated, "\n"); idx > 0 {
			truncated = truncated[:idx]
		}
		content = truncated + "\n\n> WARNING: MEMORY.md exceeded 25KB and was truncated."
	}

	return content
}

// managedTools snapshots MCP + LSP tools under w.mu so Agent.tools()
// doesn't race WarmUp / InitMCP.
func (w *Workspace) managedTools() (mcpTools, lspTools []tool.Tool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	mcpTools = append([]tool.Tool(nil), w.mcpTools...)
	lspTools = append([]tool.Tool(nil), w.lspTools...)
	return
}

func isGitRepo(dir string) bool {
	_, err := git.PlainOpen(dir)
	return err == nil
}

// isSupportedWorkspace returns true for git repos, or non-repos whose tree
// walk completes within a wall-clock budget (huge dirs like $HOME bail
// out so the UI falls back to chat + file browsing).
func isSupportedWorkspace(dir string) bool {
	if isGitRepo(dir) {
		return true
	}

	const budget = 4 * time.Second
	done := make(chan struct{})

	go func() {
		filepath.WalkDir(dir, func(_ string, _ fs.DirEntry, _ error) error {
			return nil
		})
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(budget):
		return false
	}
}

func projectMemoryDir(workingDir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}

	sanitized := filepath.Clean(workingDir)

	if vol := filepath.VolumeName(sanitized); vol != "" {
		sanitized = strings.TrimPrefix(sanitized, vol)
	}

	sanitized = strings.TrimPrefix(sanitized, string(filepath.Separator))
	sanitized = strings.ReplaceAll(sanitized, string(filepath.Separator), "_")
	sanitized = strings.ToLower(sanitized)

	return filepath.Join(home, ".wingman", "projects", sanitized, "memory")
}

func SessionsDir(workingDir string) string {
	return filepath.Join(filepath.Dir(projectMemoryDir(workingDir)), "sessions")
}

func loadBundledSkills() []skill.Skill {
	skills, _ := skill.LoadBundled(bundledFS, "skills")
	return skills
}
