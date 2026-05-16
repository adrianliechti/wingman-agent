package fs

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const DefaultListLimit = 500

func LsTool(root *os.Root) tool.Tool {
	return tool.Tool{
		Name:   "ls",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: fmt.Sprintf(
			"List a directory's immediate contents (alphabetical, '/' suffix for dirs, dotfiles included). Capped at %d entries or %dKB. Use only to inspect an unknown directory shape; prefer `grep` for content/symbol discovery and `find` for filename patterns.",
			DefaultListLimit,
			DefaultMaxBytes/1024,
		),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string", "description": "Directory; defaults to workspace."},
				"limit": map[string]any{"type": "integer", "description": "Max entries."},
			},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pathArg := "."

			if p, ok := args["path"].(string); ok && p != "" {
				pathArg = p
			}

			workingDir := root.Name()

			normalizedPath, err := ensurePathInWorkspace(pathArg, workingDir, "list directory")

			if err != nil {
				return "", err
			}

			limit := positiveIntArg(args, "limit", DefaultListLimit)

			info, err := root.Stat(normalizedPath)

			if err != nil {
				return "", pathError("stat path", pathArg, normalizedPath, workingDir, err)
			}

			if !info.IsDir() {
				return "", fmt.Errorf("path is not a directory: %s", pathArg)
			}

			dir, err := root.Open(normalizedPath)

			if err != nil {
				return "", pathError("open directory", pathArg, normalizedPath, workingDir, err)
			}
			defer dir.Close()

			entries, err := dir.ReadDir(-1)

			if err != nil {
				return "", fmt.Errorf("failed to read directory: %w", err)
			}

			if len(entries) == 0 {
				return "(empty directory)", nil
			}

			sort.Slice(entries, func(i, j int) bool {
				return entries[i].Name() < entries[j].Name()
			})

			var names []string

			for _, entry := range entries {
				if len(names) >= limit {
					break
				}

				name := entry.Name()

				if entry.IsDir() {
					name += "/"
				}

				names = append(names, name)
			}

			output := strings.Join(names, "\n")
			output, truncated := truncateHead(output)

			var notices []string

			if len(entries) > limit {
				notices = append(notices, fmt.Sprintf("limit %d hit", limit))
			}

			if truncated {
				notices = append(notices, fmt.Sprintf("%dKB cap", DefaultMaxBytes/1024))
			}

			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, "; ") + "]"
			}

			return output, nil
		},
	}
}
