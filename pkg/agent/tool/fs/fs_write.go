package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// WriteTool returns the file-write tool. allowedWriteRoots are absolute
// paths outside the workspace that this tool is additionally permitted to
// write to (e.g. the project's memory directory). Anything outside both
// the workspace and the allow-list is rejected.
func WriteTool(root *os.Root, allowedWriteRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "write",
		Effect: tool.StaticEffect(tool.EffectMutates),

		Description: strings.Join([]string{
			"Write a file to the local filesystem. Creates parent directories as needed and overwrites an existing file at the same path.",
			"- For existing files, read first so you do not discard content.",
			"- Prefer `edit` for existing files: it sends only the diff. Use `write` for new files or complete rewrites.",
			"- Prefer editing existing files unless a new file is required by the task or local pattern.",
			"- Do not create *.md / README files unless asked. No emoji unless requested.",
			"- Path may be workspace-relative or absolute inside an allowed write root.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path to write; relative to workspace or absolute inside an allowed write root."},
				"content": map[string]any{"type": "string", "description": "Complete file contents to write."},
			},
			"required":             []string{"path", "content"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pathArg, ok := args["path"].(string)

			if !ok || pathArg == "" {
				return "", fmt.Errorf("path is required")
			}

			content, ok := args["content"].(string)
			if !ok {
				return "", fmt.Errorf("content is required")
			}

			workingDir := root.Name()
			target, err := resolveWriteTarget(pathArg, workingDir, allowedWriteRoots, "write file")
			if err != nil {
				return "", err
			}

			if target.InWorkspace {
				isNew := true
				if info, err := root.Stat(target.RelPath); err == nil {
					if info.IsDir() {
						return "", fmt.Errorf("cannot write file: path %q is a directory", pathArg)
					}
					isNew = false
				} else if !os.IsNotExist(err) {
					return "", pathError("stat file", pathArg, target.RelPath, workingDir, err)
				}

				if err := writeRootFile(root, target.RelPath, content); err != nil {
					return "", pathError("write file", pathArg, target.RelPath, workingDir, err)
				}

				action := "Updated"
				if isNew {
					action = "Created"
				}
				return fmt.Sprintf("%s %s (%d bytes written)", action, pathArg, len(content)), nil
			}

			isNew := true
			if info, err := os.Stat(target.AbsPath); err == nil {
				if info.IsDir() {
					return "", fmt.Errorf("cannot write file: path %q is a directory", pathArg)
				}
				isNew = false
			} else if !os.IsNotExist(err) {
				return "", fmt.Errorf("stat file %q: %w", pathArg, err)
			}

			if err := os.MkdirAll(filepath.Dir(target.AbsPath), 0755); err != nil {
				return "", fmt.Errorf("create parent directory for %q: %w", pathArg, err)
			}
			if err := os.WriteFile(target.AbsPath, []byte(content), 0644); err != nil {
				return "", fmt.Errorf("write file %q: %w", pathArg, err)
			}

			action := "Updated"
			if isNew {
				action = "Created"
			}
			return fmt.Sprintf("%s %s (%d bytes written)", action, pathArg, len(content)), nil
		},
	}
}
