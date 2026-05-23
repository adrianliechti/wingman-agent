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
	"gopkg.in/yaml.v3"

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

const memoryMaxBytes = 25 * 1024

// UI is the elicitation hook a frontend provides for tool ask/confirm
// prompts. Pass nil to NewAgent for safe defaults (Confirm → true, Ask → "").
// The session id that triggered the prompt is on ctx via
// [SessionIDFromContext] for UIs that route prompts per session.
type UI interface {
	Ask(ctx context.Context, message string) (string, error)
	Confirm(ctx context.Context, message string) (bool, error)
}

type sessionCtxKey struct{}

// WithSessionID stamps sid onto ctx so downstream tool calls can recover
// it via [SessionIDFromContext]. The coder agent does this in Send so
// elicitation UIs can route prompts back to the right session.
func WithSessionID(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, sid)
}

// SessionIDFromContext returns the session id set by [WithSessionID],
// or "" when none is present.
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
	// LSP and Rewind are set by WarmUp; nil for unsupported workspaces.
	LSP    *lsp.Manager
	Rewind *rewind.Manager

	warmupOnce sync.Once

	mu       sync.Mutex
	mcpTools []tool.Tool
	lspTools []tool.Tool

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

// MemoryContent renders an index of the memory dir for injection into the
// system prompt: one line per `*.md` file with a short hook. The hook is
// the file's frontmatter `description`, falling back to the first non-empty
// line of the body (heading markers stripped). Cached by per-file mtime so
// repeat turns don't re-read.
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

// extractMemoryHook returns a one-line hook for the index: the frontmatter
// `description` when set, otherwise the first non-empty line of the body
// with leading `#`s stripped.
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
		if err := yaml.Unmarshal([]byte(fmBody), &fm); err == nil {
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

// splitFrontmatter returns the YAML frontmatter body and the post-fence
// body when text begins with a `---`-fenced block.
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

// ManagedTools snapshots MCP + LSP tools under the workspace mutex so a
// concurrent WarmUp / InitMCP can't race the per-turn tool() callback.
// Exported for use by the wingman sub-package, which builds each
// session's tool set from baseTools + ManagedTools().
func (w *Workspace) ManagedTools() (mcpTools, lspTools []tool.Tool) {
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
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		filepath.WalkDir(dir, func(_ string, _ fs.DirEntry, _ error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		})
	}()

	select {
	case <-done:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

// projectKey returns the canonical project identifier used in the
// ~/.wingman/projects/{key}/ path. When workingDir is inside a git
// worktree, the key derives from the canonical (main) repo root so all
// worktrees of one repo share memory and sessions. Outside git, the raw
// workingDir is used.
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

func projectMemoryDir(workingDir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}

	return filepath.Join(home, ".wingman", "projects", projectKey(workingDir), "memory")
}

func SessionsDir(workingDir string) string {
	return filepath.Join(filepath.Dir(projectMemoryDir(workingDir)), "sessions")
}

// findCanonicalGitRoot walks up from dir looking for a `.git` entry.
// If `.git` is a directory, the containing dir is the canonical root.
// If `.git` is a file (worktree pointer), it follows the gitdir reference
// and reads the worktree's `commondir` to locate the main repo's .git;
// the canonical root is the parent of that. Returns "" when no git
// metadata is found or the pointer chain is malformed.
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

	// Standard worktree layout writes commondir next to HEAD; it points
	// (usually relatively) at the main repo's .git directory.
	if data, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		common := strings.TrimSpace(string(data))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
		return filepath.Dir(filepath.Clean(common))
	}

	// Fallback: assume <main>/.git/worktrees/<name>. Walk up two parents
	// (worktrees → .git) and verify the last hop is `.git`.
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
