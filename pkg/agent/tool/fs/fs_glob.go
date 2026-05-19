package fs

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const DefaultGlobLimit = 100

func GlobTool(root *os.Root, allowedReadRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "glob",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			"- Fast file pattern matching tool that works with any codebase size.",
			"- Supports glob patterns like `**/*.js` or `src/**/*.ts`.",
			"- Returns matching file paths sorted by modification time (oldest first).",
			"- Use this tool when you need to find files by name patterns. Use `grep` for content/symbols.",
			"- Symlinks and version-control directories (`.git`, `.svn`, …) are skipped. All other files (including dotfiles) are listed; exclude them with a more specific pattern.",
			"- For open-ended searches requiring multiple rounds, use the `agent` tool.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "The glob pattern to match files against."},
				"path":    map[string]any{"type": "string", "description": "The directory to search in. Defaults to workspace root. Must be a valid directory path if provided."},
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

			target, pattern, err := resolveGlobSearch(searchDir, pattern, workingDir, root, allowedReadRoots)
			if err != nil {
				return "", err
			}
			defer target.Close()

			if _, err := doublestar.Match(pattern, ""); err != nil {
				return "", fmt.Errorf("invalid glob pattern: %w", err)
			}

			info, err := target.Root.Stat(target.SearchDirFS)

			if err != nil {
				return "", fmt.Errorf("stat path %q: %w", searchDir, err)
			}

			if !info.IsDir() {
				return "", fmt.Errorf("path is not a directory: %s", searchDir)
			}

			fsys := vcsFilteredFS{target.Root.FS()}

			type fileResult struct {
				path    string
				modTime time.Time
			}
			var results []fileResult

			// GlobWalk traverses fsys rooted at fs.FS root, so the pattern
			// must include the SearchDirFS prefix when not at the root.
			fullPattern := pattern
			if target.SearchDirFS != "." && target.SearchDirFS != "" {
				fullPattern = target.SearchDirFS + "/" + pattern
			}

			err = doublestar.GlobWalk(fsys, fullPattern, func(p string, d fs.DirEntry) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				results = append(results, fileResult{path: target.ReportPath(p), modTime: entryModTime(d)})
				return nil
			}, doublestar.WithFilesOnly(), doublestar.WithNoFollow())

			if err != nil && err != filepath.SkipAll {
				return "", fmt.Errorf("failed to search directory: %w", err)
			}

			totalMatches := len(results)

			if totalMatches == 0 {
				return "No files found", nil
			}

			// Oldest mtime first; lexical path as a stable tiebreaker.
			slices.SortFunc(results, func(a, b fileResult) int {
				return cmp.Or(a.modTime.Compare(b.modTime), cmp.Compare(a.path, b.path))
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

			output := strings.Join(paths, "\n")

			if resultLimitReached {
				output += "\n(Results are truncated. Consider using a more specific path or pattern.)"
			}

			return output, nil
		},
	}
}

func resolveGlobSearch(searchDir, pattern, workingDir string, workspaceRoot *os.Root, allowedReadRoots []string) (*searchTarget, string, error) {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))

	// An absolute pattern overrides searchDir: split it into (dir, glob) and
	// route the dir through the same allow-list as a `path` argument.
	if filepath.IsAbs(pattern) {
		dir, rest := doublestar.SplitPattern(pattern)
		target, err := resolveSearchTarget(filepath.FromSlash(dir), workingDir, workspaceRoot, allowedReadRoots, "search")
		if err != nil {
			return nil, "", err
		}
		return target, normalizeGlobPattern(rest), nil
	}

	target, err := resolveSearchTarget(searchDir, workingDir, workspaceRoot, allowedReadRoots, "search")
	if err != nil {
		return nil, "", err
	}
	return target, normalizeGlobPattern(pattern), nil
}

// vcsFilteredFS is a fs.FS wrapper that hides version-control directories
// (.git, .svn, …) from doublestar.GlobWalk. Mirrors claude's
// `--glob !.git` exclusions without disabling general dotfile matching.
type vcsFilteredFS struct{ fs.FS }

func (f vcsFilteredFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, name)
	if err != nil {
		return nil, err
	}
	filtered := entries[:0]
	for _, e := range entries {
		if e.IsDir() && vcsDirs[e.Name()] {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered, nil
}

func normalizeGlobPattern(pattern string) string {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" || strings.Contains(pattern, "/") {
		return pattern
	}
	return "**/" + pattern
}
