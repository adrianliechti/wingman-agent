// Package sandboxpolicy defines the filesystem boundary shared by command and
// file tools. Platform sandboxes enforce it independently of model decisions.
package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
)

func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WINGMAN_SANDBOX"))) {
	case "off", "false", "0", "no", "disabled":
		return false
	default:
		return true
	}
}

// Git metadata is protected as a whole, including worktree pointer files.
var ProtectedDirs = []string{".git", ".vscode", ".idea", ".codex", ".wingman", ".agents", ".claude"}
var ProtectedFiles = []string{".envrc"}
var ProtectedRootFiles = []string{"mcp.json", ".mcp.json", ".gitmodules", ".bashrc", ".bash_profile", ".zshrc", ".zprofile", ".profile"}

func Canonical(path string) string {
	if resolved, err := pathutil.ResolveExistingPrefix(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func ProtectedRelative(path string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		path = strings.ToLower(path)
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		for _, name := range ProtectedDirs {
			if part == name {
				return true
			}
		}
		for _, name := range ProtectedFiles {
			if part == name {
				return true
			}
		}
	}
	for _, name := range ProtectedRootFiles {
		if path == name {
			return true
		}
	}
	return false
}

// CheckWrite checks both the requested spelling and its resolved destination.
// It also covers new descendants of an existing symlinked directory.
func CheckWrite(root, path string) error {
	if !Enabled() {
		return nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	for _, pair := range [][2]string{{filepath.Clean(root), filepath.Clean(path)}, {Canonical(root), Canonical(path)}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return fmt.Errorf("resolve protected path: %w", err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if ProtectedRelative(rel) {
			return fmt.Errorf("cannot modify protected configuration %q; use an explicitly approved shell command if this change is required", path)
		}
	}
	// An existing protected path may be a link to an otherwise ordinary name.
	for _, names := range [][]string{ProtectedDirs, ProtectedFiles, ProtectedRootFiles} {
		for _, name := range names {
			protected := filepath.Join(root, name)
			if _, err := os.Lstat(protected); err != nil {
				continue
			}
			rel, err := filepath.Rel(Canonical(protected), Canonical(path))
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("cannot modify protected configuration %q through alias %q", protected, path)
			}
		}
	}
	return nil
}
