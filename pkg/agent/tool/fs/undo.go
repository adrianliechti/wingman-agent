package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// UndoFileChanges preserves the contents that preceded the agent's first
// edit, including uncommitted user changes. It refuses discontinuous edit
// chains and any file whose current contents or mode changed afterwards.
func UndoFileChanges(ctx context.Context, root *os.Root, changes []tool.FileChange) error {
	fileEditMu.Lock()
	defer fileEditMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	files := make([]tool.FileChange, 0, len(changes))
	indexes := map[string]int{}
	for _, change := range changes {
		if !filepath.IsLocal(change.Path) {
			return fmt.Errorf("cannot undo a change outside the workspace: %s", change.Path)
		}
		path := filepath.Clean(filepath.FromSlash(change.Path))
		// Undo never follows a substituted symlink, even within the workspace.
		for part := path; part != "."; part = filepath.Dir(part) {
			info, err := root.Lstat(part)
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("cannot undo through symlink: %s", part)
			}
			if err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if index, ok := indexes[path]; ok {
			previous := &files[index]
			if previous.After != change.Before || previous.AfterExists != change.BeforeExists || previous.Mode != change.Mode {
				return fmt.Errorf("cannot undo %s: another edit occurred between the agent's edits", path)
			}
			previous.After, previous.AfterExists = change.After, change.AfterExists
		} else {
			indexes[path] = len(files)
			change.Path = path
			files = append(files, change)
		}
	}
	tx := &fileTransaction{}
	for _, file := range files {
		target, err := resolveFileTarget(file.Path, root.Name(), nil, "undo edit")
		if err != nil {
			return err
		}
		tx.changes = append(tx.changes, fileChange{target: target, displayPath: file.Path, before: []byte(file.After), beforePresent: file.AfterExists, beforeMode: os.FileMode(file.Mode), after: []byte(file.Before), afterPresent: file.BeforeExists})
	}
	return commitFileTransaction(ctx, root, tx)
}
