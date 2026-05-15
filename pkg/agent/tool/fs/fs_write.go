package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func WriteTool(root *os.Root) tool.Tool {
	return tool.Tool{
		Name:   "write",
		Effect: tool.StaticEffect(tool.EffectMutates),

		Description: strings.Join([]string{
			"Create or **overwrite** a file. Creates parent directories as needed.",
			"- For existing files, `read` first. For partial changes prefer `edit` — smaller diffs, cheaper, and reviewable.",
			"- Use this only for new files or complete rewrites.",
			"- Do not create *.md / README files unless the user asked for them. No emoji unless requested.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path."},
				"content": map[string]any{"type": "string", "description": "File contents."},
			},
			"required": []string{"path", "content"},
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

			_, existsErr := root.ReadFile(normalizedPath)
			isNew := existsErr != nil

			dir := filepath.Dir(normalizedPath)

			if dir != "." && dir != "" {
				if err := root.MkdirAll(dir, 0755); err != nil {
					return "", pathError("create directory", pathArg, normalizedPath, workingDir, err)
				}
			}

			file, err := root.Create(normalizedPath)

			if err != nil {
				return "", pathError("create file", pathArg, normalizedPath, workingDir, err)
			}

			if _, err := file.WriteString(content); err != nil {
				file.Close()
				return "", fmt.Errorf("failed to write file: %w", err)
			}

			if err := file.Close(); err != nil {
				return "", fmt.Errorf("failed to close file: %w", err)
			}

			action := "Updated"
			if isNew {
				action = "Created"
			}

			result := fmt.Sprintf("%s %s (%d bytes)", action, pathArg, len(content))

			return result, nil
		},
	}
}
