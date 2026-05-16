package fs

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const DefaultGlobLimit = 100

func GlobTool(root *os.Root) tool.Tool {
	return tool.Tool{
		Name:   "glob",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			fmt.Sprintf("Fast filename search using glob patterns like `**/*.js` or `src/**/*.ts`. Returns workspace-relative paths sorted by modification time, limited to %d results.", DefaultGlobLimit),
			"- Use for finding files by name or wildcard. Use `grep` for file contents, symbols, errors, TODOs, or config keys.",
			"- Includes hidden and gitignored files, similar to `rg --files --hidden --no-ignore`; skips VCS directories.",
			"- For open-ended searches requiring multiple rounds, use the `agent` tool.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "The glob pattern to match files against."},
				"path":    map[string]any{"type": "string", "description": "The directory to search in. Omit this field to use the workspace root. Must be a valid directory path if provided."},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pattern, ok := args["pattern"].(string)

			if !ok || strings.TrimSpace(pattern) == "" {
				return "", fmt.Errorf("pattern is required")
			}
			searchDir := "."

			if p, ok := args["path"].(string); ok && p != "" {
				searchDir = p
			}

			workingDir := root.Name()

			searchDirFS, pattern, err := resolveGlobSearch(searchDir, pattern, workingDir)

			if err != nil {
				return "", err
			}
			if _, err := doublestar.Match(pattern, ""); err != nil {
				return "", fmt.Errorf("invalid glob pattern: %w", err)
			}

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

			err = walkAllFiles(ctx, fsys, searchDirFS, func(path, relPath string) error {
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
					results = append(results, fileResult{path: filepath.FromSlash(path), modTime: modTime})
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

			sort.Slice(results, func(i, j int) bool {
				if results[i].modTime.Equal(results[j].modTime) {
					return results[i].path < results[j].path
				}
				return results[i].modTime.Before(results[j].modTime)
			})

			end := totalMatches
			resultLimitReached := false
			if DefaultGlobLimit < end {
				end = DefaultGlobLimit
				resultLimitReached = true
			}
			results = results[:end]

			paths := make([]string, len(results))
			for i, r := range results {
				paths[i] = r.path
			}

			rawOutput := strings.Join(paths, "\n")

			if resultLimitReached {
				rawOutput += "\n(Results are truncated. Consider using a more specific path or pattern.)"
			}

			truncatedOutput, truncated := truncateHead(rawOutput)
			if truncated {
				truncatedOutput += fmt.Sprintf("\n(%dKB cap reached.)", DefaultMaxBytes/1024)
			}

			return truncatedOutput, nil
		},
	}
}

func resolveGlobSearch(searchDir, pattern, workingDir string) (string, string, error) {
	searchDirFS, err := ensurePathInWorkspaceFS(searchDir, workingDir, "search")
	if err != nil {
		return "", "", err
	}

	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if filepath.IsAbs(pattern) {
		rel, ok := relPathWithinWorkspace(pattern, workingDir)
		if !ok {
			return "", "", fmt.Errorf("cannot search: pattern %q is outside workspace %q", pattern, workingDir)
		}

		base, relativePattern := extractGlobBaseDirectory(filepath.ToSlash(rel))
		searchDirFS = base
		if searchDirFS == "" {
			searchDirFS = "."
		}
		pattern = relativePattern
	}

	return searchDirFS, normalizeGlobPattern(pattern), nil
}

func extractGlobBaseDirectory(pattern string) (string, string) {
	first := strings.IndexAny(pattern, "*?{[")
	if first == -1 {
		return filepath.ToSlash(filepath.Dir(pattern)), filepath.Base(pattern)
	}

	staticPrefix := pattern[:first]
	lastSlash := strings.LastIndex(staticPrefix, "/")
	if lastSlash == -1 {
		return "", pattern
	}

	return pattern[:lastSlash], pattern[lastSlash+1:]
}

func walkAllFiles(ctx context.Context, fsys fs.FS, root string, onFile func(path, relPath string) error) error {
	return fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if d.IsDir() {
			if vcsDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		return onFile(path, relPathFromBase(root, path))
	})
}

func normalizeGlobPattern(pattern string) string {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" || strings.Contains(pattern, "/") {
		return pattern
	}
	return "**/" + pattern
}
