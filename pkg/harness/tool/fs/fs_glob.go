package fs

import (
	"cmp"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

const DefaultGlobLimit = 100

func GlobTool(root *os.Root, allowedReadRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "glob",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			"- Find files by name using glob patterns.",
			"- Supports glob patterns like `**/*.js` or `src/**/*.ts`.",
			"- Basename patterns like `*.go` search recursively. Relative patterns containing `/` are relative to `path`; absolute patterns override `path` and keep their specified depth.",
			"- Returns up to 100 matching file paths sorted by modification time (newest first), then by path for ties. Narrow the path or pattern when results are truncated.",
			"- Use this tool when you need to find files by name patterns. Use `grep` for content/symbols.",
			"- Symlinks encountered while matching and version-control directories (`.git`, `.svn`, …) are skipped. An explicitly requested search directory may use an allowed directory link. Dotfiles and gitignored files are included.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "The glob pattern to match files against."},
				"path":    map[string]any{"type": "string", "description": "Literal directory to search, within the workspace or an allowed read root. Defaults to workspace root; ignored for absolute patterns."},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}
			pattern, ok := args["pattern"].(string)

			if !ok || strings.TrimSpace(pattern) == "" {
				return tool.Result{}, fmt.Errorf("pattern is required")
			}
			searchDir := "."

			if value, exists := args["path"]; exists && value != nil {
				p, ok := value.(string)
				if !ok {
					return tool.Result{}, fmt.Errorf("path must be a string")
				}
				if p != "" {
					searchDir = p
				}
			}

			workingDir := root.Name()

			target, pattern, absolutePattern, err := resolveGlobSearch(searchDir, pattern, workingDir, root, allowedReadRoots)
			if err != nil {
				return tool.Result{}, err
			}
			defer target.Close()

			if _, err := doublestar.Match(pattern, ""); err != nil {
				return tool.Result{}, fmt.Errorf("invalid glob pattern: %w", err)
			}

			for component := range strings.SplitSeq(target.SearchDirFS, "/") {
				if vcsDirs[component] {
					return tool.Text("No files found"), nil
				}
			}

			info, err := target.Root.Stat(target.SearchDirFS)

			if err != nil {
				if absolutePattern && errors.Is(err, fs.ErrNotExist) {
					return tool.Text("No files found"), nil
				}
				return tool.Result{}, fmt.Errorf("stat path %q: %w", searchDir, err)
			}

			if !info.IsDir() {
				if absolutePattern {
					return tool.Text("No files found"), nil
				}
				return tool.Result{}, fmt.Errorf("path is not a directory: %s", searchDir)
			}
			// Open the literal search directory separately so brackets, braces,
			// and other glob metacharacters in its name are never interpreted.
			searchRoot, err := target.Root.OpenRoot(target.SearchDirFS)
			if err != nil {
				return tool.Result{}, fmt.Errorf("open search directory: %w", err)
			}
			defer searchRoot.Close()
			fsys := globSearchFS{root: searchRoot, ctx: ctx}
			results := make(globResults, 0, DefaultGlobLimit)
			totalMatches := 0

			err = doublestar.GlobWalk(fsys, pattern, func(p string, d fs.DirEntry) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				info, err := d.Info()
				if errors.Is(err, fs.ErrNotExist) {
					return nil // A file removed while walking is no longer a match.
				}
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return nil
				}
				totalMatches++
				result := globFile{path: target.ReportPath(path.Join(target.SearchDirFS, p)), modTime: info.ModTime()}
				if len(results) < DefaultGlobLimit {
					heap.Push(&results, result)
				} else if compareGlobFiles(result, results[0]) < 0 {
					results[0] = result
					heap.Fix(&results, 0)
				}
				return nil
			}, doublestar.WithFilesOnly(), doublestar.WithNoFollow(), doublestar.WithFailOnIOErrors())

			if err != nil {
				return tool.Result{}, fmt.Errorf("failed to search directory: %w", err)
			}
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}

			if totalMatches == 0 {
				return tool.Text("No files found"), nil
			}

			slices.SortFunc(results, compareGlobFiles)

			paths := make([]string, len(results))
			for i, r := range results {
				paths[i] = r.path
			}

			output := strings.Join(paths, "\n")

			if totalMatches > len(results) {
				output += "\n(Results are truncated. Consider using a more specific path or pattern.)"
			}

			return tool.Text(output), nil
		},
	}
}

func resolveGlobSearch(searchDir, pattern, workingDir string, workspaceRoot *os.Root, allowedReadRoots []string) (*searchTarget, string, bool, error) {
	rawPattern := anchorRootedPath(expandHome(normalizePathArg(pattern)), workingDir)
	pattern = filepath.ToSlash(rawPattern)

	if filepath.IsAbs(rawPattern) {
		dir, rest := splitAbsoluteGlobPattern(rawPattern, pattern, workingDir, allowedReadRoots)
		target, err := resolveSearchTarget(dir, workingDir, workspaceRoot, allowedReadRoots, "search")
		if err != nil {
			return nil, "", true, err
		}
		return target, rest, true, nil
	}

	target, err := resolveSearchTarget(searchDir, workingDir, workspaceRoot, allowedReadRoots, "search")
	if err != nil {
		return nil, "", false, err
	}
	return target, normalizeGlobPattern(pattern), false, nil
}

func splitAbsoluteGlobPattern(rawPattern, slashPattern, workingDir string, allowedReadRoots []string) (string, string) {
	if rel, ok := relPathWithinWorkspace(rawPattern, workingDir); ok && rel != "." {
		subDir, rest := doublestar.SplitPattern(filepath.ToSlash(rel))
		return filepath.Join(workingDir, filepath.FromSlash(subDir)), rest
	}
	if rootClean, sub, ok := matchExplicitRoot(rawPattern, workingDir, allowedReadRoots); ok && sub != "" {
		subDir, rest := doublestar.SplitPattern(filepath.ToSlash(sub))
		return filepath.Join(rootClean, filepath.FromSlash(subDir)), rest
	}
	dir, rest := doublestar.SplitPattern(slashPattern)
	return filepath.FromSlash(dir), rest
}

type globFile struct {
	path    string
	modTime time.Time
}

func compareGlobFiles(a, b globFile) int {
	return cmp.Or(b.modTime.Compare(a.modTime), cmp.Compare(a.path, b.path))
}

// Keep only the best 100 matches, with the worst retained match at the root.
type globResults []globFile

func (r globResults) Len() int           { return len(r) }
func (r globResults) Less(i, j int) bool { return compareGlobFiles(r[i], r[j]) > 0 }
func (r globResults) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }
func (r *globResults) Push(value any)    { *r = append(*r, value.(globFile)) }
func (r *globResults) Pop() any {
	last := (*r)[len(*r)-1]
	*r = (*r)[:len(*r)-1]
	return last
}

// Check every filesystem access, including literal pattern prefixes that
// doublestar can otherwise follow without visiting their directory entries.
type globSearchFS struct {
	root *os.Root
	ctx  context.Context
}

func (f globSearchFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	var info fs.FileInfo
	prefix := ""
	for component := range strings.SplitSeq(name, "/") {
		if err := f.ctx.Err(); err != nil {
			return nil, err
		}
		prefix = path.Join(prefix, component)
		var err error
		info, err = f.root.Lstat(prefix)
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 || (info.IsDir() && vcsDirs[component]) {
			return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
		}
	}
	return info, nil
}

func (f globSearchFS) Open(name string) (fs.File, error) {
	if _, err := f.Stat(name); err != nil {
		return nil, err
	}
	return f.root.Open(name)
}

func (f globSearchFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if _, err := f.Stat(name); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(f.root.FS(), name)
	if err != nil {
		return nil, err
	}
	filtered := entries[:0]
	for _, e := range entries {
		if err := f.ctx.Err(); err != nil {
			return nil, err
		}
		if e.Type()&fs.ModeSymlink != 0 || (e.IsDir() && vcsDirs[e.Name()]) {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered, nil
}

func normalizeGlobPattern(pattern string) string {
	pattern = filepath.ToSlash(pattern)
	recursive := !strings.Contains(pattern, "/")
	for strings.HasPrefix(pattern, "./") {
		pattern = strings.TrimPrefix(pattern, "./")
	}
	if pattern == "" || !recursive {
		return pattern
	}
	return "**/" + pattern
}
