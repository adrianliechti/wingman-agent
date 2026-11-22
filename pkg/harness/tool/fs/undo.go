package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

// UndoFileChanges preserves the contents that preceded the agent's first
// edit, including uncommitted user changes. It refuses discontinuous edit
// chains and any file whose current contents or mode changed afterwards.
func UndoFileChanges(ctx context.Context, root *os.Root, changes []tool.FileChange, allowedWriteRoots ...string) error {
	fileEditMu.Lock()
	defer fileEditMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, change := range changes {
		if !filepath.IsLocal(change.Path) && !(filepath.IsAbs(change.Path) && len(allowedWriteRoots) > 0) {
			return fmt.Errorf("cannot undo a change outside the workspace: %s", change.Path)
		}
	}
	files, gap := tool.NetFileChanges(changes)
	if gap != "" {
		return fmt.Errorf("cannot undo %s: another edit occurred between the agent's edits", gap)
	}
	// For an inverse move the destination supplies the original bytes.
	movedTo := make(map[string]string)
	for _, file := range files {
		if file.MovedFrom != "" && file.AfterRef != nil {
			movedTo[filepath.Clean(file.MovedFrom)] = file.Path
		}
	}
	tx := &fileTransaction{}
	for _, file := range files {
		// An intermediate name can belong to new, unrelated work by now.
		if file.Unchanged() {
			continue
		}
		target, err := resolveFileTarget(file.Path, root.Name(), allowedWriteRoots, "undo edit")
		if err != nil {
			return err
		}
		if err := checkUndoPath(root, target); err != nil {
			return err
		}
		mode := os.FileMode(file.Mode)
		tx.changes = append(tx.changes, fileChange{
			target:        target,
			displayPath:   file.Path,
			movedFrom:     movedTo[file.Path],
			before:        []byte(file.After),
			beforeRef:     file.AfterRef,
			beforePresent: file.AfterExists,
			beforeMode:    os.FileMode(file.ResultMode()),
			after:         []byte(file.Before),
			afterRef:      file.BeforeRef,
			afterPresent:  file.BeforeExists,
			afterMode:     &mode,
		})
	}
	return commitFileTransactionLocked(ctx, root, tx)
}

// Undo never follows a substituted symlink, even within a writable root.
func checkUndoPath(workspace *os.Root, target fileTarget) error {
	root, path, closeRoot, err := fileTargetRoot(workspace, target)
	if err != nil {
		return err
	}
	if closeRoot != nil {
		defer closeRoot()
	}
	for part := filepath.Clean(path); part != "."; part = filepath.Dir(part) {
		info, err := root.Lstat(part)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("cannot undo through symlink: %s", part)
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
