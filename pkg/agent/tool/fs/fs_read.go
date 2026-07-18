package fs

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func ReadTool(root *os.Root, allowedReadRoots ...string) tool.Tool {
	return readTool(root, nil, nil, allowedReadRoots...)
}

func readTool(root *os.Root, tracker *contentTracker, freshness *Freshness, allowedReadRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "read",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			fmt.Sprintf("Reads a file from the local filesystem. Results use cat -n format with 1-based line numbers. By default reads the first %d lines; output is capped at %dKB, with a trailing notice telling you which offset to continue from.", DefaultMaxLines, DefaultMaxBytes/1024),
			"- Use `offset` and `limit` for long files or known ranges. `offset` is a 1-based start line, not a result skip count.",
			"- Reads files only, not directories. Use `glob` to list files in a directory.",
			"- Binary files (PDF, images, archives) are rejected. SVG files are treated as text. Use `view_image` to look at image files.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{"type": "string", "description": "The absolute path to the file to read."},
				"offset":    map[string]any{"type": "integer", "description": "1-based line number to start reading from. Only provide for large files or known ranges. Defaults to 1."},
				"limit":     map[string]any{"type": "integer", "description": "Positive number of lines to read. Only provide for large files or known ranges."},
			},
			"required":             []string{"file_path"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pathArg, ok := args["file_path"].(string)

			if !ok || pathArg == "" {
				return "", fmt.Errorf("file_path is required")
			}

			workingDir := root.Name()

			limit := 0
			if v, present, err := tool.PositiveIntArg(args, "limit"); present {
				if err != nil {
					return "", err
				}
				limit = v
			}

			startLine := 1
			if v, present, err := tool.PositiveIntArg(args, "offset"); present {
				if err != nil {
					return "", fmt.Errorf("offset must be a positive 1-based integer")
				}
				startLine = v
			}

			target, err := resolveFileTarget(pathArg, workingDir, allowedReadRoots, "read file")
			if err != nil {
				return "", err
			}

			info, err := statFileTarget(root, target)
			if err != nil {
				return "", fmt.Errorf("stat file %q: %w", pathArg, err)
			}
			if info.IsDir() {
				return "", fmt.Errorf("cannot read file: path %q is a directory; use glob to find files inside it", pathArg)
			}

			content, err := readFileTarget(root, target)
			if err != nil {
				return "", fmt.Errorf("read file %q: %w", pathArg, err)
			}

			if isBinaryContent(content) {
				return "", fmt.Errorf("cannot read %s: file appears to be binary. Use the shell tool with an appropriate viewer if you really need to inspect it", pathArg)
			}

			tracker.record(content)
			freshness.record(ctx, target)

			return formatRead(content, startLine, limit)
		},
	}
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
	if limit > 0 {
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
