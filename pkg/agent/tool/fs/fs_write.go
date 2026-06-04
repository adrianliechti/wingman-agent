package fs

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func WriteTool(root *os.Root, allowedWriteRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "write",
		Effect: tool.StaticEffect(tool.EffectMutates),

		Description: strings.Join([]string{
			"Writes a file to the local filesystem. Creates parent directories as needed and overwrites any existing file at the same path.",
			"- For existing files, `read` first so you do not discard content.",
			"- Prefer `edit` for existing files: it sends only the diff. Use `write` for new files or complete rewrites.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{"type": "string", "description": "The absolute path to the file to write (must be absolute, not relative)."},
				"content":   map[string]any{"type": "string", "description": "The content to write to the file."},
			},
			"required":             []string{"file_path", "content"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pathArg, ok := args["file_path"].(string)

			if !ok || pathArg == "" {
				return "", fmt.Errorf("file_path is required")
			}

			content, ok := args["content"].(string)
			if !ok {
				return "", fmt.Errorf("content is required")
			}

			workingDir := root.Name()
			target, err := resolveFileTarget(pathArg, workingDir, allowedWriteRoots, "write file")
			if err != nil {
				return "", err
			}

			isNew := true
			info, err := statFileTarget(root, target)
			switch {
			case err == nil:
				if info.IsDir() {
					return "", fmt.Errorf("cannot write file: path %q is a directory", pathArg)
				}
				isNew = false
			case !os.IsNotExist(err):
				return "", fmt.Errorf("stat file %q: %w", pathArg, err)
			}

			if err := writeFileTarget(root, target, content); err != nil {
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
