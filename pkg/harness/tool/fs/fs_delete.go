package fs

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func DeleteTool(root *os.Root, allowedWriteRoots ...string) tool.Tool {
	return deleteTool(root, nil, allowedWriteRoots...)
}

// deleteTool runs deletions through the edit transaction, so validation, rollback, and change tracking are shared.
// Deletion is its own tool because models that write arguments in schema order cannot reach a flag on an edit entry.
func deleteTool(root *os.Root, freshness *Freshness, allowedWriteRoots ...string) tool.Tool {
	return tool.Tool{
		Name:        "delete",
		Effect:      tool.StaticEffect(tool.EffectMutates),
		Description: "Delete one or more UTF-8 text files (up to 10 MiB each). To remove a package or directory, list its files together in file_paths. Empty parent directories are removed automatically. Use literal paths to regular files. File symlinks are refused. If validation fails, no files change.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_paths": map[string]any{
					"type":        "array",
					"minItems":    1,
					"items":       map[string]any{"type": "string"},
					"description": writePathScope,
				},
			},
			"required":             []string{"file_paths"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			paths, _ := args["file_paths"].([]any)
			if len(paths) == 0 {
				return tool.Result{}, fmt.Errorf("file_paths must be a non-empty array")
			}
			guard := sandboxpolicy.NewCredentialGuard(root.Name())
			entries := make([]parsedBatchEntry, 0, len(paths))
			for index, value := range paths {
				if err := ctx.Err(); err != nil {
					return tool.Result{}, err
				}
				path, ok := value.(string)
				argument := fmt.Sprintf("file_paths[%d]", index)
				if !ok || strings.TrimSpace(path) == "" {
					return tool.Result{}, fmt.Errorf("%s must be a path (no files changed)", argument)
				}
				op := editOp{deleteFile: true}
				target, err := mutationSource(path, root.Name(), allowedWriteRoots, guard, op)
				if err != nil {
					return tool.Result{}, fmt.Errorf("%s: %w (no files changed)", argument, err)
				}
				entries = append(entries, parsedBatchEntry{argument: argument, target: target, displayPath: target.displayPath(), op: op})
			}
			return applyFileEdits(ctx, root, freshness, guard, entries, true)
		},
	}
}
