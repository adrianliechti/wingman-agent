package fs

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func WriteTool(root *os.Root) tool.Tool {
	return tool.Tool{
		Name:   "write",
		Effect: tool.StaticEffect(tool.EffectMutates),

		Description: strings.Join([]string{
			"Write a file to the local filesystem. Creates parent directories as needed and overwrites an existing file at the same path.",
			"- For existing files, read first so you do not discard content.",
			"- Prefer `edit` for existing files: it sends only the diff. Use `write` for new files or complete rewrites.",
			"- Prefer editing existing files unless a new file is required by the task or local pattern.",
			"- Do not create *.md / README files unless asked. No emoji unless requested.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path to write; relative to workspace or absolute inside the workspace."},
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

			workingDir := root.Name()

			normalizedPath, err := ensurePathInWorkspace(pathArg, workingDir, "write file")

			if err != nil {
				return "", err
			}

			content, ok := args["content"].(string)

			if !ok {
				return "", fmt.Errorf("content is required")
			}

			isNew := true
			if info, err := root.Stat(normalizedPath); err == nil {
				if info.IsDir() {
					return "", fmt.Errorf("cannot write file: path %q is a directory", pathArg)
				}
				isNew = false
			} else if !os.IsNotExist(err) {
				return "", pathError("stat file", pathArg, normalizedPath, workingDir, err)
			}

			if err := writeRootFile(root, normalizedPath, content); err != nil {
				return "", pathError("write file", pathArg, normalizedPath, workingDir, err)
			}

			action := "Updated"
			if isNew {
				action = "Created"
			}

			result := fmt.Sprintf("%s %s (%d bytes written)", action, pathArg, len(content))

			return result, nil
		},
	}
}
