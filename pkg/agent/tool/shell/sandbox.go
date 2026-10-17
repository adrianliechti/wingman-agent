package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/layout"
)

const (
	workspaceSandboxActiveEnv  = "WINGMAN_SANDBOX_ACTIVE"
	workspaceSandboxNetworkEnv = "WINGMAN_SANDBOX_NETWORK"
)

// sandboxPolicy is what a platform backend enforces for one command.
type sandboxPolicy struct {
	WritableRoots []string
	// ProtectedRoot keeps git, editor, and agent configuration read-only beneath the workspace.
	ProtectedRoot string
	// DenyRead hides existing credential paths.
	DenyRead []string
	Network  bool
}

// Writes here would later run code outside the sandbox: through git, editors, direnv, shells, or agent config.
var (
	protectedDirs      = []string{".git/hooks", ".vscode", ".idea", ".codex", ".wingman", ".agents", ".claude"}
	protectedFiles     = []string{".git/config", ".envrc"}
	protectedRootFiles = []string{"mcp.json", ".mcp.json", ".gitmodules", ".bashrc", ".bash_profile", ".zshrc", ".zprofile", ".profile"}
)

func existingProtectedPaths(root string) []string {
	if root == "" {
		return nil
	}
	var paths []string
	for _, name := range slices.Concat(protectedDirs, protectedFiles, protectedRootFiles) {
		path := filepath.Join(root, name)
		if _, err := os.Lstat(path); err == nil {
			paths = append(paths, path)
		}
	}
	return paths
}

func sandboxNetworkEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(workspaceSandboxNetworkEnv)), "off")
}

// credentialPaths returns the existing credential stores sandboxed commands must not read.
func credentialPaths() []string {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, rel := range []string{".ssh", ".aws", ".gnupg", ".azure", ".kube", ".netrc", ".git-credentials", ".docker/config.json", ".config/gh", ".config/gcloud"} {
			candidates = append(candidates, filepath.Join(home, rel))
		}
	}
	if path, err := layout.WingmanPath("mcp-credentials.json"); err == nil {
		candidates = append(candidates, path)
	}
	var paths []string
	for _, path := range candidates {
		path = canonicalPath(path)
		if _, err := os.Lstat(path); err == nil && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	return paths
}

// workspaceSandboxEnabled is deliberately opt-in while the native backends
// mature. WINGMAN_SANDBOX=off keeps its existing meaning for the file tools;
// workspace (or workspace-write) adds an OS boundary around shell processes.
func workspaceSandboxEnabled() bool {
	// Native sandboxes are inherited by descendants. Avoid trying to apply a
	// second Seatbelt profile or Bubblewrap namespace when a sandboxed command
	// starts another Wingman shell runner.
	if os.Getenv(workspaceSandboxActiveEnv) == "1" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WINGMAN_SANDBOX"))) {
	case "workspace", "workspace-write":
		return true
	default:
		return false
	}
}

func pathWithin(root, candidate string) bool {
	root = canonicalPath(root)
	candidate = canonicalPath(candidate)

	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func canonicalPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func buildSandboxedCommand(ctx context.Context, shell, command, workingDir string, opts sandboxOptions) (*exec.Cmd, error) {
	workspaceDir := canonicalPath(opts.WorkspaceDir)
	workingDir = canonicalPath(workingDir)
	for label, path := range map[string]string{
		"workspace": workspaceDir,
		"workdir":   workingDir,
	} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("sandbox %s %q is not an accessible directory", label, path)
		}
	}
	if !pathWithin(workspaceDir, workingDir) {
		return nil, fmt.Errorf("workdir %q is outside sandbox workspace %q", workingDir, workspaceDir)
	}

	shellPath, err := exec.LookPath(shell)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox shell %q: %w", shell, err)
	}
	// Keep the link name: multi-call binaries such as busybox dispatch on argv[0].
	if shell, err = filepath.Abs(shellPath); err != nil {
		return nil, fmt.Errorf("resolve sandbox shell %q: %w", shellPath, err)
	}

	policy := sandboxPolicy{
		WritableRoots: writableRootsFor(workspaceDir, opts.ExtraWritableRoots),
		ProtectedRoot: workspaceDir,
		DenyRead:      credentialPaths(),
		Network:       sandboxNetworkEnabled(),
	}
	path, args, err := platformSandboxCommand(shell, command, workingDir, policy)
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, path, args...), nil
}

// writableRootsFor assembles the sandbox's writable roots: the workspace,
// any extra roots the caller needs (e.g. the memory directory), and the OS
// temp dirs every command may stage files under. Entries that don't resolve
// to an accessible directory are dropped rather than failing the command —
// an extra root is a nice-to-have, not a precondition like the workspace.
func writableRootsFor(workspaceDir string, extraWritableRoots []string) []string {
	writableRoots := []string{canonicalPath(workspaceDir)}
	candidates := append(append([]string{}, extraWritableRoots...), os.TempDir())
	if runtime.GOOS != "windows" {
		candidates = append(candidates, "/tmp")
	}
	for _, path := range candidates {
		if path == "" {
			continue
		}
		path = canonicalPath(path)
		if info, err := os.Stat(path); err == nil && info.IsDir() && !slices.Contains(writableRoots, path) {
			writableRoots = append(writableRoots, path)
		}
	}
	return writableRoots
}
