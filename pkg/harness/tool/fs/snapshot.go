package fs

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

const (
	maxSnapshotFiles      = 8_000
	maxSnapshotTotalBytes = 64 * 1024 * 1024
	maxSnapshotFileBytes  = MaxEditFileBytes
)

type snapshotEntry struct {
	mode    os.FileMode
	content []byte
}

// WorkspaceSnapshot captures a bounded baseline of non-ignored UTF-8 workspace
// files so mutations made by shell commands (`exec` / `exec_session`) can be
// attributed to the turn and reversed by UndoFileChanges.
type WorkspaceSnapshot struct {
	files      map[string]snapshotEntry
	complete   bool
	enumerated bool
	skipped    map[string]bool
}

// CaptureWorkspaceSnapshot walks workspaceRoot respecting .gitignore and hidden
// credential filters. If the workspace exceeds file/byte caps, Complete() is
// false so callers know untracked changes may still exist.
func CaptureWorkspaceSnapshot(ctx context.Context, root *os.Root) (*WorkspaceSnapshot, error) {
	if root == nil {
		return &WorkspaceSnapshot{complete: false}, nil
	}
	fsys := root.FS()
	credentials := sandboxpolicy.NewCredentialGuard(root.Name())
	resolvedRoot := sandboxpolicy.Canonical(root.Name())
	hidden := func(dir, name string, info func() (os.FileInfo, error)) bool {
		return credentials.Named(name) && credentials.Hides(filepath.Join(resolvedRoot, filepath.FromSlash(dir), name), info)
	}
	files := make(map[string]snapshotEntry, 256)
	var totalBytes int64
	complete := true
	enumerated := true
	visited := 0
	skipped := make(map[string]bool)

	err := walkGrepFilesWithSkipped(ctx, fsys, ".", hidden, func(path, _ string, d fs.DirEntry) error {
		visited++
		if visited > maxSnapshotFiles {
			complete, enumerated = false, false
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			skipped[path] = true
			complete = false
			return nil
		}
		if info.Size() > maxSnapshotFileBytes {
			skipped[path] = true
			complete = false
			return nil
		}
		if totalBytes+info.Size() > maxSnapshotTotalBytes {
			complete, enumerated = false, false
			return fs.SkipAll
		}
		f, err := fsys.Open(path)
		if err != nil {
			skipped[path] = true
			complete = false
			return nil
		}
		data, readErr := readContent(ctx, f, maxSnapshotFileBytes)
		_ = f.Close()
		if readErr != nil || int64(len(data)) > maxSnapshotFileBytes {
			skipped[path] = true
			complete = false
			return nil
		}
		if isBinaryContent(data) || !utf8.Valid(data) {
			skipped[path] = true
			complete = false
			return nil
		}
		totalBytes += int64(len(data))
		clean := filepath.ToSlash(pathpkg.Clean(path))
		files[clean] = snapshotEntry{
			mode:    info.Mode().Perm(),
			content: data,
		}
		return nil
	}, func(path string) { skipped[path] = true })
	if err != nil {
		return nil, err
	}
	return &WorkspaceSnapshot{files: files, complete: complete, enumerated: enumerated, skipped: skipped}, nil
}

// Complete reports whether every eligible workspace file fit within snapshot limits.
func (s *WorkspaceSnapshot) Complete() bool {
	return s != nil && s.complete
}

// absent distinguishes a missing file from a path omitted by filters or limits.
func (s *WorkspaceSnapshot) absent(path string) bool {
	if !s.enumerated {
		return false
	}
	for p := path; p != "."; p = pathpkg.Dir(p) {
		if s.skipped[p] {
			return false
		}
	}
	_, exists := s.files[path]
	return !exists
}

// Diff compares s (before) against after and returns structured FileChanges in
// deterministic path order. It deliberately leaves read baselines alone: the
// model has not seen the new content, so the changed-on-disk notice and the
// stale-edit guard must still fire for files a command rewrote.
func (s *WorkspaceSnapshot) Diff(after *WorkspaceSnapshot) ([]tool.FileChange, bool) {
	if s == nil || after == nil {
		return nil, false
	}
	seen := make(map[string]struct{}, len(s.files)+len(after.files))
	for k := range s.files {
		seen[k] = struct{}{}
	}
	for k := range after.files {
		seen[k] = struct{}{}
	}
	paths := slices.Sorted(maps.Keys(seen))

	var changes []tool.FileChange
	complete := s.complete && after.complete
	for _, rel := range paths {
		pre, hadPre := s.files[rel]
		post, hasPost := after.files[rel]
		switch {
		case hadPre && hasPost:
			if bytes.Equal(pre.content, post.content) && samePermissions(pre.mode, post.mode) {
				continue
			}
			changes = append(changes, tool.FileChange{
				Path:         rel,
				Before:       string(pre.content),
				After:        string(post.content),
				BeforeExists: true,
				AfterExists:  true,
				Mode:         uint32(pre.mode),
			})
			if !samePermissions(pre.mode, post.mode) {
				mode := uint32(post.mode)
				changes[len(changes)-1].AfterMode = &mode
			}
		case !hadPre && hasPost:
			if !s.absent(rel) {
				complete = false
				continue
			}
			changes = append(changes, tool.FileChange{
				Path:         rel,
				After:        string(post.content),
				BeforeExists: false,
				AfterExists:  true,
				Mode:         uint32(post.mode),
			})
		case hadPre && !hasPost:
			if !after.absent(rel) {
				complete = false
				continue
			}
			changes = append(changes, tool.FileChange{
				Path:         rel,
				Before:       string(pre.content),
				BeforeExists: true,
				AfterExists:  false,
				Mode:         uint32(pre.mode),
			})
		}
	}
	return changes, complete
}

// WrapWithWorkspaceSnapshot wraps shell execution tools so workspace text-file
// mutations (formatters, codegen, file moves/removals) are recorded as
// durable tool.FileChange entries for turn reviews and undo.
func WrapWithWorkspaceSnapshot(root *os.Root, tools []tool.Tool) []tool.Tool {
	if root == nil {
		return tools
	}
	wrapped := slices.Clone(tools)
	for i := range wrapped {
		name := wrapped[i].Name
		if name != "exec" && name != "exec_command" && name != "exec_session" {
			continue
		}
		execute := wrapped[i].Execute
		if execute == nil {
			continue
		}
		eff := wrapped[i].Effect
		wrapped[i].Execute = func(ctx context.Context, args map[string]any) (tool.Result, error) {
			if eff != nil && eff(args) == tool.EffectReadOnly {
				return execute(ctx, args)
			}
			before, preErr := CaptureWorkspaceSnapshot(ctx, root)
			out, execErr := execute(ctx, args)
			if execErr != nil || preErr != nil {
				return out, execErr
			}
			// Do not claim complete tracking when a background session is still running.
			_, hasSession := tool.IntArg(out.Metadata, "session_id")
			_, hasExit := out.Metadata["exit_code"]
			stillRunning := hasSession && !hasExit

			after, postErr := CaptureWorkspaceSnapshot(ctx, root)
			if postErr != nil {
				return out, nil
			}
			changes, complete := before.Diff(after)
			if len(changes) == 0 && (!complete || stillRunning) {
				return out, nil
			}
			meta := maps.Clone(out.Metadata)
			if meta == nil {
				meta = make(map[string]any, 2)
			}
			if len(changes) > 0 {
				meta[tool.FileChangesMetadata] = changes
			}
			if complete && !stillRunning {
				meta["workspace_snapshot_complete"] = true
			}
			out.Metadata = meta
			return out, nil
		}
	}
	return wrapped
}
