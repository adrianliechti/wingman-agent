package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const DefaultFindLimit = 100

func FindTool(root *os.Root) tool.Tool {
	return tool.Tool{
		Name:   "find",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			fmt.Sprintf("Find files by glob pattern (e.g., `*.go`, `**/*.{ts,tsx}`). Sorted newest-first. Respects .gitignore. Default limit %d.", DefaultFindLimit),
			"- Patterns without a slash, like `*.go`, match recursively anywhere under `path`. Use an explicit subtree in `path` to narrow the search.",
			"- Use `grep` instead when searching by content — it already returns matching file paths.",
			"- `limit`/`offset` paginate newest-first results. If the file you want is older than recent activity, narrow the pattern or increase offset.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob (e.g. `*.go`, `**/*.{ts,tsx}`). Patterns without a slash match recursively."},
				"path":    map[string]any{"type": "string", "description": "Search root; defaults to workspace."},
				"limit":   map[string]any{"type": "integer", "description": fmt.Sprintf("Max results (default %d).", DefaultFindLimit)},
				"offset":  map[string]any{"type": "integer", "description": "0-based number of newest-first results to skip before applying limit."},
			},
			"required": []string{"pattern"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pattern, ok := args["pattern"].(string)

			if !ok || strings.TrimSpace(pattern) == "" {
				return "", fmt.Errorf("pattern is required")
			}
			pattern = normalizeFindPattern(pattern)
			if _, err := doublestar.Match(pattern, ""); err != nil {
				return "", fmt.Errorf("invalid glob pattern: %w", err)
			}

			searchDir := "."

			if p, ok := args["path"].(string); ok && p != "" {
				searchDir = p
			}

			workingDir := root.Name()

			searchDirFS, err := ensurePathInWorkspaceFS(searchDir, workingDir, "search")

			if err != nil {
				return "", err
			}

			limit := positiveIntArg(args, "limit", DefaultFindLimit)
			offset := positiveIntArg(args, "offset", 0)

			info, err := root.Stat(searchDirFS)

			if err != nil {
				return "", pathError("stat path", searchDir, searchDirFS, workingDir, err)
			}

			if !info.IsDir() {
				return "", fmt.Errorf("path is not a directory: %s", searchDir)
			}

			fsys := root.FS()

			type fileResult struct {
				path    string
				modTime time.Time
			}
			var results []fileResult

			err = walkWorkspace(ctx, fsys, searchDirFS, func(path, relPath string) error {
				matched, err := doublestar.Match(pattern, relPath)

				if err != nil {
					return nil
				}

				if matched {
					var modTime time.Time
					if fi, err := fsys.Open(path); err == nil {
						if stat, err := fi.Stat(); err == nil {
							modTime = stat.ModTime()
						}
						fi.Close()
					}
					results = append(results, fileResult{path: filepath.FromSlash(relPath), modTime: modTime})
				}

				return nil
			})

			if err != nil && err != filepath.SkipAll {
				return "", fmt.Errorf("failed to search directory: %w", err)
			}

			totalMatches := len(results)

			if totalMatches == 0 {
				return "No files found matching pattern", nil
			}

			// Sort by modification time (newest first). The walk visits every match
			// before sorting so that the "newest" promise actually holds — a top-N
			// from a partial walk would just be the newest among the first files
			// visited, which depends on filesystem order.
			sort.Slice(results, func(i, j int) bool {
				return results[i].modTime.After(results[j].modTime)
			})

			if offset >= totalMatches {
				return fmt.Sprintf("No files found matching pattern (offset %d beyond %d results)", offset, totalMatches), nil
			}

			start := offset
			end := totalMatches
			resultLimitReached := false
			if start+limit < end {
				end = start + limit
				resultLimitReached = true
			}
			results = results[start:end]

			paths := make([]string, len(results))
			for i, r := range results {
				paths[i] = r.path
			}

			rawOutput := strings.Join(paths, "\n")
			truncatedOutput, truncated := truncateHead(rawOutput)

			var notices []string

			if resultLimitReached {
				notices = append(notices, fmt.Sprintf("%d found, showing %d from offset %d; offset=%d for more", totalMatches, len(results), offset, end))
			} else {
				notices = append(notices, fmt.Sprintf("%d found, showing %d from offset %d", totalMatches, len(results), offset))
			}

			if truncated {
				notices = append(notices, fmt.Sprintf("%dKB cap", DefaultMaxBytes/1024))
			}

			truncatedOutput += "\n\n[" + strings.Join(notices, "; ") + "]"

			return truncatedOutput, nil
		},
	}
}

func normalizeFindPattern(pattern string) string {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" || strings.Contains(pattern, "/") {
		return pattern
	}
	return "**/" + pattern
}
