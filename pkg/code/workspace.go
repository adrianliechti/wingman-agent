package code

import (
	"context"
	"embed"
	"errors"
	"fmt"
	iofs "io/fs"
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

// UI is the interface a frontend must provide to the coding agent.
type UI interface {
	Ask(ctx context.Context, message string) (string, error)
	Confirm(ctx context.Context, message string) (bool, error)
	StatusUpdate(status string)
}

// Workspace owns the resources shared by every Agent (conversation) bound
// to a working directory: the filesystem root, MCP/LSP/Rewind services,
// and the skills catalog. It does NOT own the API client — that lives on
// *agent.Config, separated so Workspace stays a pure workdir handle.
//
// One Workspace can back many Agents: the server holds one Workspace per
// running instance plus an *agent.Config (the API template), and produces
// per-session Agents via Workspace.NewAgent(cfg, ui).
type Workspace struct {
	Root        *os.Root
	RootPath    string
	MemoryPath  string
	ScratchPath string

	Skills []skill.Skill

	MCP *mcp.Manager
	// LSP is set by WarmUp when the workspace is a supported git repo;
	// nil otherwise. Callers nil-check before use.
	LSP *lsp.Manager
	// Rewind is set by WarmUp when the workspace is supported (git repo or
	// small enough to walk in time); nil otherwise.
	Rewind *rewind.Manager

	warmupOnce sync.Once

	mu       sync.Mutex
	mcpTools []tool.Tool
	lspTools []tool.Tool
}

// NewWorkspace opens the working directory, discovers skills, allocates a
// scratch dir, and loads the MCP config. Heavy probes (Rewind/LSP setup,
// MCP connect) happen lazily in WarmUp/InitMCP.
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

	// Skill precedence (later overrides earlier):
	//   bundled  → shipped with the binary, hidden from catalog until invoked
	//   personal → ~/.claude/skills, ~/.wingman/skills (user-wide)
	//   project  → .claude, .wingman, .skills, .github, .opencode (this repo)
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

// WarmUp runs the slow workspace probe and initializes Rewind/LSP if the
// directory is supported. Idempotent: the first caller does the work,
// subsequent callers block on sync.Once until it finishes.
//
// Three resulting modes:
//
//   - supported git repo  → Rewind set, LSP set, lspTools set
//   - supported scratch   → Rewind set, LSP nil, lspTools nil
//   - unsupported (huge)  → Rewind nil, LSP nil; UI falls back to chat-only
func (w *Workspace) WarmUp() {
	w.warmupOnce.Do(func() {
		if !isSupportedWorkspace(w.RootPath) {
			return
		}

		// Sweep stale shadow repos from prior crashed sessions.
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

// InitMCP connects MCP servers and fetches their tools. Call this after
// the UI is ready (typically async).
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

// Close tears down every workspace-owned resource.
func (w *Workspace) Close() {
	if w == nil {
		return
	}
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

// IsGitRepo reports whether the working directory is currently a git repo.
// Re-evaluated on each call so callers can react to `git init` / `rm -rf
// .git` happening mid-session.
func (w *Workspace) IsGitRepo() bool { return isGitRepo(w.RootPath) }

// SyncProjectMode rebuilds LSP when the working dir's git status flips
// (typically: agent ran `git init` in a scratch dir). No-op on unsupported
// workspaces — `git init` alone doesn't make a 1M-file home folder small
// enough to support full features.
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

// Diagnostics returns the workspace's current diagnostics keyed by file
// path. Empty if LSP isn't running (huge dir / non-git workspace) — that
// folds the nil-check that every caller would otherwise duplicate.
func (w *Workspace) Diagnostics(ctx context.Context) map[string][]lsp.Diagnostic {
	if w.LSP == nil {
		return nil
	}
	return w.LSP.CollectAllDiagnostics(ctx)
}

// Checkpoint operations. Each is a no-op (or returns nil/empty) when
// Rewind isn't running, so call sites don't need to duplicate the
// nil-check. Use w.Rewind directly for poll-loop primitives like
// Fingerprint that aren't user-facing.

// Commit snapshots the current worktree under msg. No-op if checkpoints
// aren't being tracked.
func (w *Workspace) Commit(msg string) error {
	if w.Rewind == nil {
		return nil
	}
	return w.Rewind.Commit(msg)
}

// Diffs returns the changes from the current checkpoint baseline.
func (w *Workspace) Diffs() ([]rewind.FileDiff, error) {
	if w.Rewind == nil {
		return nil, nil
	}
	return w.Rewind.DiffFromBaseline()
}

// Checkpoints lists committed checkpoints, newest last.
func (w *Workspace) Checkpoints() ([]rewind.Checkpoint, error) {
	if w.Rewind == nil {
		return nil, nil
	}
	return w.Rewind.List()
}

// Restore rolls the worktree back to the named checkpoint.
func (w *Workspace) Restore(hash string) error {
	if w.Rewind == nil {
		return errors.New("checkpoint tracking is not available for this workspace")
	}
	return w.Rewind.Restore(hash)
}

// MemoryContent reads MEMORY.md, trimming to memoryMaxBytes.
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

// managedTools snapshots MCP + LSP tools under w.mu so Agent.tools() can
// build its tool list without racing WarmUp / InitMCP.
func (w *Workspace) managedTools() (mcpTools, lspTools []tool.Tool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	mcpTools = append([]tool.Tool(nil), w.mcpTools...)
	lspTools = append([]tool.Tool(nil), w.lspTools...)
	return
}

// isGitRepo reports whether dir is the root of a real git working tree.
// Used as the single project-mode gate for LSP detection and rewind init —
// keeping the predicate in one place ensures both features agree on what
// "this is a project" means.
func isGitRepo(dir string) bool {
	_, err := git.PlainOpen(dir)
	return err == nil
}

// isSupportedWorkspace decides whether wingman's heavier features (rewind,
// LSP, diffs panel) should run for this directory. A real git repo is
// always supported. Otherwise we walk the tree with a wall-clock budget:
// if it finishes in time the directory is small enough; if it doesn't,
// classify as unsupported and let the UI fall back to chat + file browsing.
func isSupportedWorkspace(dir string) bool {
	if isGitRepo(dir) {
		return true
	}

	const budget = 4 * time.Second
	done := make(chan struct{})

	go func() {
		filepath.WalkDir(dir, func(_ string, _ iofs.DirEntry, _ error) error {
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

func loadBundledSkills() []skill.Skill {
	skills, _ := skill.LoadBundled(bundledFS, "skills")
	return skills
}
