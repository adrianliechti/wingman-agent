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
			fmt.Sprintf("Read a known file path. Results use cat -n format with 1-based line numbers. Defaults to the first %d lines.", DefaultMaxLines),
			"- Required before `edit` on the same file.",
			"- Do not use for discovery: use `grep` for content/symbols and `glob` for filename patterns.",
			"- After `grep` finds candidates, read only the file or line window needed for context.",
			"- Use `offset` and `limit` for long files or known ranges. `offset` is a 1-based start line, not a result skip count.",
			"- Do not re-read a file already shown unless it changed; use the existing line numbers.",
			"- Path may be workspace-relative or absolute inside an allowed root. `~/` expands to home.",
			"- Reads files only, not directories. Use `glob` to find files in a directory.",
			"- Binary files (PDF, images, archives) are rejected.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File path; relative to workspace or absolute inside an allowed root. `~/` expands to home."},
				"offset": map[string]any{"type": "integer", "description": "1-based line number to start reading from. Only provide for large files or known ranges. Defaults to 1."},
				"limit":  map[string]any{"type": "integer", "description": "Positive number of lines to read. Only provide for large files or known ranges."},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
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

			limit := 0
			if l, present, err := tool.OptionalIntArg(args, "limit"); present {
				if err != nil {
					return "", fmt.Errorf("limit must be a positive integer")
				}
				if l <= 0 {
					return "", fmt.Errorf("limit must be a positive integer")
				}
				limit = l
			}

			startLine := 1

			if o, present, err := tool.OptionalIntArg(args, "offset"); present {
				if err != nil || o <= 0 {
					return "", fmt.Errorf("offset must be a positive 1-based integer")
				}
				startLine = o
			}

			content, err := readFromAllowedLocation(root, workingDir, expanded, allowedReadRoots)
			if err != nil {
				return "", err
			}

			return formatRead(content, startLine, limit)
		},
	}
}

func readFromAllowedLocation(root *os.Root, workingDir, path string, allowedRoots []string) ([]byte, error) {
	if !isOutsideWorkspace(path, workingDir) {
		normalizedPath := normalizePath(path, workingDir)
		info, err := root.Stat(normalizedPath)
		if err != nil {
			return nil, pathError("stat file", path, normalizedPath, workingDir, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("cannot read file: path %q is a directory; use glob to find files inside it", path)
		}
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
			info, err := os.Stat(cleaned)
			if err != nil {
				return nil, fmt.Errorf("stat file %q: %w", path, err)
			}
			if info.IsDir() {
				return nil, fmt.Errorf("cannot read file: path %q is a directory; use glob to find files inside it", path)
			}
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

func formatRead(content []byte, startLine, limit int) (string, error) {
	if len(content) == 0 {
		return "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>", nil
	}

	_, text := stripBom(string(content))
	lines := strings.Split(normalizeToLF(text), "\n")
	total := len(lines)
	offset := startLine - 1

	if offset >= total {
		return fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", startLine, total), nil
	}

	maxLines := DefaultMaxLines
	if limit > 0 && limit < maxLines {
		maxLines = limit
	}

	end := min(total, offset+maxLines)

	var numbered []string

	for i, line := range lines[offset:end] {
		lineNum := offset + i + 1
		numbered = append(numbered, fmt.Sprintf("%d\t%s", lineNum, line))
	}

	selected := strings.Join(numbered, "\n")
	output, bytesTruncated := truncateReadOutput(selected)

	outputLines := 0
	if output != "" {
		outputLines = strings.Count(output, "\n") + 1
	}
	endLine := offset + outputLines

	if bytesTruncated || end < total {
		notice := fmt.Sprintf("Showing lines %d-%d of %d", startLine, endLine, total)
		if bytesTruncated {
			notice += fmt.Sprintf("; %dKB cap reached", DefaultMaxBytes/1024)
		}
		if endLine < total {
			notice += fmt.Sprintf("; use offset=%d to continue", endLine+1)
		}
		return fmt.Sprintf("%s\n\n[%s]", output, notice), nil
	}

	return output, nil
}

func truncateReadOutput(content string) (string, bool) {
	if len(content) <= DefaultMaxBytes {
		return content, false
	}

	cut := strings.LastIndex(content[:DefaultMaxBytes], "\n")
	if cut <= 0 {
		return content[:DefaultMaxBytes], true
	}

	return content[:cut], true
}
