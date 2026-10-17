package fs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

func ReadTool(root *os.Root, allowedReadRoots ...string) tool.Tool {
	return readTool(root, nil, 0, allowedReadRoots...)
}

func readTool(root *os.Root, freshness *Freshness, maxFileBytes int64, allowedReadRoots ...string) tool.Tool {
	if maxFileBytes == 0 {
		maxFileBytes = MaxReadFileBytes
	}

	return tool.Tool{
		Name:   "read",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			fmt.Sprintf("Reads a file from the local filesystem. Text results use cat -n format with 1-based line numbers. By default reads the first %d lines; text output is capped at %dKB, with a trailing notice telling you which offset to continue from.", DefaultMaxLines, DefaultMaxBytes/1024),
			fmt.Sprintf("- PNG, JPEG, GIF, and WebP images are attached so you can see them (screenshots, mockups, diagrams, etc.). Sources up to %dMB and 64 megapixels are supported; large images are resized to fit attachment limits. Resized GIFs show the first frame; animated WebP must already fit attachment limits. offset and limit apply only to text, not images.", tool.MaxImageSourceBytes/(1024*1024)),
			"- PDF, RTF, supported Word, Excel, and PowerPoint documents (including macro-enabled files, templates, and slide shows), EML, and Outlook MSG files are converted to Markdown. `offset` and `limit` select lines from that Markdown just like any text file.",
			"- Use `offset` and `limit` for long files or known ranges. `offset` is a 1-based start line, not a result skip count.",
			"- Reads files only, not directories. Use `glob` to list files in a directory.",
			"- Other binary files are rejected. SVG and HTML files are treated as text.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{"type": "string", "description": "Workspace-relative path, absolute workspace path, or path in an explicitly allowed read root."},
				"offset":    map[string]any{"type": "integer", "minimum": 1, "description": "1-based line number to start reading from, including for converted documents. Only provide for large files or known ranges. Defaults to 1."},
				"limit":     map[string]any{"type": "integer", "minimum": 1, "description": "Positive number of lines to read, including from converted documents. Only provide for large files or known ranges."},
			},
			"required":             []string{"file_path"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}
			pathArg, ok := args["file_path"].(string)

			if !ok || pathArg == "" {
				return tool.Result{}, fmt.Errorf("file_path is required")
			}

			workingDir := root.Name()

			limit := 0
			if v, present, err := tool.PositiveIntArg(args, "limit"); present {
				if err != nil {
					return tool.Result{}, err
				}
				limit = v
			}

			startLine := 1
			if v, present, err := tool.PositiveIntArg(args, "offset"); present {
				if err != nil {
					return tool.Result{}, fmt.Errorf("offset must be a positive 1-based integer")
				}
				startLine = v
			}

			target, err := resolveFileTarget(pathArg, workingDir, allowedReadRoots, "read file")
			if err != nil {
				return tool.Result{}, err
			}

			info, err := statFileTarget(root, target)
			if err != nil {
				return tool.Result{}, fmt.Errorf("stat file %q: %w", pathArg, err)
			}
			if info.IsDir() {
				return tool.Result{}, fmt.Errorf("cannot read file: path %q is a directory; use glob to find files inside it", pathArg)
			}
			if !info.Mode().IsRegular() {
				return tool.Result{}, fmt.Errorf("cannot read file: path %q is not a regular file", pathArg)
			}
			f, err := openFileTarget(root, target)
			if err != nil {
				return tool.Result{}, fmt.Errorf("read file %q: %w", pathArg, err)
			}
			defer f.Close()
			reader := bufio.NewReader(f)
			head, err := reader.Peek(512)
			if err != nil && err != io.EOF {
				return tool.Result{}, fmt.Errorf("read file %q: %w", pathArg, err)
			}
			// Detect images before document hints and text streaming, including
			// when an image has no extension or exceeds the text read threshold.
			if tool.ImageMIME(head) != "" {
				if info.Size() > tool.MaxImageSourceBytes {
					return tool.Result{}, fmt.Errorf("cannot read image %q: exceeds the %dMB source limit", pathArg, tool.MaxImageSourceBytes/(1024*1024))
				}
				result, err := tool.ReadImage(ctx, reader)
				if err != nil {
					return tool.Result{}, fmt.Errorf("read image %q: %w", pathArg, err)
				}
				result.Metadata["file_path"] = pathArg
				freshness.recordInfo(ctx, target, info)
				return result, nil
			}
			documentHint := documentPath(pathArg) || documentData(head)
			if documentHint && info.Size() > MaxDocumentBytes {
				return tool.Result{}, fmt.Errorf("cannot read document: %q is %.1fMB (max %dMB)", pathArg, float64(info.Size())/(1024*1024), MaxDocumentBytes/(1024*1024))
			}
			if !documentHint && maxFileBytes > 0 && info.Size() > maxFileBytes {
				output, err := readTextWindow(ctx, reader, pathArg, info.Size(), startLine, limit)
				if err != nil {
					return tool.Result{}, err
				}
				freshness.recordInfo(ctx, target, info)
				return tool.Text(output), nil
			}

			readLimit := maxFileBytes
			if documentHint {
				readLimit = MaxDocumentBytes
			}
			content, err := readContent(ctx, reader, readLimit)
			if err != nil {
				return tool.Result{}, fmt.Errorf("read file %q: %w", pathArg, err)
			}
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}

			if len(content) > MaxDocumentBytes && (documentHint || documentData(content)) {
				return tool.Result{}, fmt.Errorf("cannot read document: %q grew beyond the %dMB limit while being read", pathArg, MaxDocumentBytes/(1024*1024))
			}
			if !documentHint && maxFileBytes > 0 && int64(len(content)) > maxFileBytes {
				// The file grew after Stat. Stream the buffered prefix and the
				// unread remainder instead of continuing an unbounded allocation.
				output, err := readTextWindow(ctx, io.MultiReader(bytes.NewReader(content), reader), pathArg, int64(len(content)), startLine, limit)
				if err != nil {
					return tool.Result{}, err
				}
				freshness.recordInfo(ctx, target, info)
				return tool.Text(output), nil
			}

			if result, handled, err := readDocument(ctx, target, pathArg, content, startLine, limit, nil); handled {
				if err == nil {
					freshness.recordInfo(ctx, target, info)
				}
				return result, err
			}

			if isBinaryContent(content) {
				return tool.Result{}, fmt.Errorf("cannot read %s: file appears to be binary. Use the shell tool with an appropriate viewer if you really need to inspect it", pathArg)
			}

			freshness.recordInfo(ctx, target, info)

			return tool.Text(formatRead(content, startLine, limit)), nil
		},
	}
}

func formatRead(content []byte, startLine, limit int) string {
	return formatReadWithin(content, startLine, limit, DefaultMaxBytes)
}

func formatReadWithin(content []byte, startLine, limit, budget int) string {
	if len(content) == 0 {
		return "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>"
	}

	_, text := stripBom(string(content))
	remaining := normalizeToLF(text)
	total := strings.Count(remaining, "\n") + 1
	offset := startLine - 1

	if offset >= total {
		return fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", startLine, total)
	}

	maxLines := DefaultMaxLines
	if limit > 0 {
		maxLines = limit
	}

	end := offset + min(total-offset, maxLines)

	var numbered []string
	selectedBytes := 0

	for i := 0; i < offset; i++ {
		_, remaining, _ = strings.Cut(remaining, "\n")
	}
	for lineNum := startLine; lineNum <= end; lineNum++ {
		var line string
		line, remaining, _ = strings.Cut(remaining, "\n")
		row := fmt.Sprintf("%d\t%s", lineNum, line)
		numbered = append(numbered, row)
		selectedBytes += len(row) + 1
		if selectedBytes-1 > max(0, budget-readNoticeBytes) {
			break
		}
	}

	selected := strings.Join(numbered, "\n")
	output, bytesTruncated := truncateReadOutput(selected, budget)

	outputLines := 0
	if output != "" {
		outputLines = strings.Count(output, "\n") + 1
	}
	endLine := offset + outputLines

	if bytesTruncated || end < total {
		notice := fmt.Sprintf("Showing lines %d-%d of %d", startLine, endLine, total)
		if bytesTruncated {
			notice += fmt.Sprintf("; %dKB cap reached", DefaultMaxBytes/1024)
			if !strings.Contains(output, "\n") {
				notice += "; this line may be incomplete; use grep or exec to inspect its remainder"
			}
		}
		if endLine < total {
			notice += fmt.Sprintf("; use offset=%d to continue", endLine+1)
		}
		return fmt.Sprintf("%s\n\n[%s]", output, notice)
	}

	return output
}

const maxReadLineBytes = 512 * 1024

// readTextWindow serves files too large to load whole: it streams the file and
// keeps only the requested line window in memory.
func readTextWindow(ctx context.Context, source io.Reader, pathArg string, fileSize int64, startLine, limit int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	reader := bufio.NewReaderSize(source, 64*1024)

	head, err := reader.Peek(binarySniffLen)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read file %q: %w", pathArg, err)
	}
	if isBinaryContent(head) {
		return "", fmt.Errorf("cannot read %s: file appears to be binary. Use the shell tool with an appropriate viewer if you really need to inspect it", pathArg)
	}

	maxLines := DefaultMaxLines
	if limit > 0 {
		maxLines = limit
	}

	var numbered []string
	size := 0
	lineNum := 0
	sawEOF := false

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line, tooLong, err := readLimitedLine(reader)
		if tooLong {
			return "", fmt.Errorf("cannot read %s: line %d is longer than %dKB; use grep or the shell tool to inspect this file", pathArg, lineNum+1, maxReadLineBytes/1024)
		}
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("read file %q: %w", pathArg, err)
		}
		sawEOF = err == io.EOF

		lineNum++
		if lineNum == 1 {
			_, line = stripBom(line)
		}

		if lineNum >= startLine {
			text := strings.TrimSuffix(line, "\n")
			text = strings.TrimSuffix(text, "\r")
			row := fmt.Sprintf("%d\t%s", lineNum, text)
			numbered = append(numbered, row)
			size += len(row) + 1
		}

		if sawEOF || len(numbered) >= maxLines || size >= DefaultMaxBytes {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if len(numbered) == 0 {
		return fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", startLine, lineNum), nil
	}

	output, bytesTruncated := truncateReadOutput(strings.Join(numbered, "\n"), DefaultMaxBytes)
	outputLines := strings.Count(output, "\n") + 1
	endLine := startLine + outputLines - 1
	notice := fmt.Sprintf("Showing lines %d-%d of a %.1fMB file (too large to read fully)", startLine, endLine, float64(fileSize)/(1024*1024))
	if bytesTruncated {
		notice += fmt.Sprintf("; %dKB cap reached", DefaultMaxBytes/1024)
		if !strings.Contains(output, "\n") {
			notice += "; this line may be incomplete; use grep or exec to inspect its remainder"
		}
	}
	if !sawEOF || bytesTruncated {
		notice += fmt.Sprintf("; use offset=%d to continue", endLine+1)
	}
	return fmt.Sprintf("%s\n\n[%s]", output, notice), nil
}

// readLimitedLine recognizes LF, CRLF, and CR like normalizeToLF, while capping
// how much of an unbroken line is held in memory. Delimiters are returned as LF.
func readLimitedLine(r *bufio.Reader) (string, bool, error) {
	var line []byte
	for {
		if _, err := r.Peek(1); err != nil {
			return string(line), false, err
		}
		chunk, _ := r.Peek(r.Buffered())
		end := bytes.IndexAny(chunk, "\r\n")
		length := len(chunk)
		if end >= 0 {
			length = end
		}
		if len(line)+length > maxReadLineBytes {
			return "", true, nil
		}
		line = append(line, chunk[:length]...)
		_, _ = r.Discard(length)
		if end < 0 {
			continue
		}
		delimiter, _ := r.ReadByte()
		if delimiter == '\r' {
			next, err := r.Peek(1)
			if err != nil && err != io.EOF {
				return "", false, err
			}
			if len(next) > 0 && next[0] == '\n' {
				_, _ = r.Discard(1)
			}
		}
		return string(append(line, '\n')), false, nil
	}
}

// Bound whole-file reads as well as checking Stat: files may grow while open.
// A negative limit retains the explicit unlimited text-read option.
func readContent(ctx context.Context, source io.Reader, limit int64) ([]byte, error) {
	if limit > 0 && limit < 1<<63-1 {
		source = io.LimitReader(source, limit+1)
	}
	return io.ReadAll(contextReader{ctx: ctx, source: source})
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

// Reserve enough room for line numbers, continuation and partial-line notices.
const readNoticeBytes = 512

func truncateReadOutput(content string, budget int) (string, bool) {
	limit := max(0, budget-readNoticeBytes)
	if len(content) <= limit {
		return content, false
	}

	cut := strings.LastIndex(content[:limit], "\n")
	if cut <= 0 {
		return text.HeadBytes(content, limit), true
	}

	return content[:cut], true
}
