package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// ReadTool returns the file-read tool. allowedReadRoots are absolute paths
// outside the workspace that this tool is additionally permitted to read
// (e.g. discovered personal skill directories). Anything outside both the
// workspace and the allow-list is rejected.
func ReadTool(root *os.Root, allowedReadRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "read",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			fmt.Sprintf("Read a known file path. Output includes 1-based line numbers. Capped at %d lines / %dKB.", DefaultMaxLines, DefaultMaxBytes/1024),
			"- Required before `edit` on the same file.",
			"- Do not use for discovery. If you are looking for a symbol, keyword, config key, error text, or likely file, use `grep` first; if you need filenames by pattern, use `find`.",
			"- After `grep` finds candidate files, read only the specific file or line window needed for context.",
			"- Token efficiency: for large files or known locations, use `offset`/`limit`; `offset` is a 1-based start line, not a result skip count.",
			"- Do not re-read a file already shown in this conversation unless it changed. Use the line numbers already present.",
			"- Path may be workspace-relative or absolute inside an allowed root. `~/` expands to home.",
			"- If output is truncated, the result tells you the next `offset` to continue from.",
			"- Binary files (PDF, images, archives) are rejected — inspect with an appropriate viewer via `shell` if you must.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File path; relative to workspace or absolute inside an allowed root. `~/` expands to home."},
				"offset": map[string]any{"type": "integer", "description": "1-based start line."},
				"limit":  map[string]any{"type": "integer", "description": "Max lines to read."},
			},
			"required": []string{"path"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pathArg, ok := args["path"].(string)

			if !ok || pathArg == "" {
				return "", fmt.Errorf("path is required")
			}

			workingDir := root.Name()
			expanded := expandHome(pathArg)

			if isBinaryFile(expanded) {
				return "", fmt.Errorf("cannot read %s: file appears to be binary (extension %q). Use the shell tool with an appropriate viewer if you really need to inspect it", pathArg, filepath.Ext(expanded))
			}

			limit := positiveIntArg(args, "limit", 0)
			offset := 0

			if o, ok := optionalInt(args, "offset"); ok && o > 0 {
				offset = o - 1
			}

			content, err := readFromAllowedLocation(root, workingDir, expanded, allowedReadRoots)
			if err != nil {
				return "", err
			}

			return formatRead(content, offset, limit)
		},
	}
}

func readFromAllowedLocation(root *os.Root, workingDir, path string, allowedRoots []string) ([]byte, error) {
	if !isOutsideWorkspace(path, workingDir) {
		normalizedPath := normalizePath(path, workingDir)
		content, err := root.ReadFile(normalizedPath)
		if err != nil {
			return nil, pathError("read file", path, normalizedPath, workingDir, err)
		}
		return content, nil
	}

	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("cannot read file: relative path %q is outside workspace", path)
	}

	cleaned := cleanPath(path)
	cmpPath := normalizePathForComparison(cleaned)
	for _, allowed := range allowedRoots {
		allowedClean := cleanPath(allowed)
		cmpAllowed := normalizePathForComparison(allowedClean)
		if cmpPath == cmpAllowed || strings.HasPrefix(cmpPath, cmpAllowed+string(filepath.Separator)) {
			content, err := os.ReadFile(cleaned)
			if err != nil {
				return nil, fmt.Errorf("read file %q: %w", path, err)
			}
			return content, nil
		}
	}

	return nil, fmt.Errorf("cannot read file: path %q is outside workspace and not in any allowed root", path)
}

// expandHome resolves a leading `~` to the user's home dir. Accepts both
// `~/...` (forward slash) and `~\...` (Windows backslash) forms.
func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func formatRead(content []byte, offset, limit int) (string, error) {
	if len(content) == 0 {
		return "<system-reminder>File is empty.</system-reminder>", nil
	}

	lines := strings.Split(string(content), "\n")
	total := len(lines)

	if offset >= total {
		return fmt.Sprintf("<system-reminder>Offset %d is past end of file (%d lines). Re-issue with a valid offset.</system-reminder>", offset+1, total), nil
	}

	end := total

	if limit > 0 && offset+limit < total {
		end = offset + limit
	}

	var numbered []string

	for i, line := range lines[offset:end] {
		lineNum := offset + i + 1
		numbered = append(numbered, fmt.Sprintf("%6d\t%s", lineNum, line))
	}

	selected := strings.Join(numbered, "\n")
	output, truncated := truncateHead(selected)

	outputLines := len(strings.Split(output, "\n"))
	endLine := offset + outputLines

	if truncated || end < total {
		return fmt.Sprintf("%s\n\n[lines %d-%d/%d, offset=%d for more]", output, offset+1, endLine, total, endLine+1), nil
	}

	return output, nil
}
