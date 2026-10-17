package code

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func normalizePastedText(text string) string {
	text = ansi.Strip(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, text)
}

func normalizePastedSearchQuery(text string) string {
	return strings.Join(strings.Fields(normalizePastedText(text)), " ")
}

func appendPastedSearchQuery(current, pasted string) string {
	pasted = normalizePastedSearchQuery(pasted)
	if pasted == "" {
		return current
	}
	if current != "" && strings.TrimRightFunc(current, unicode.IsSpace) == current {
		current += " "
	}
	return current + pasted
}

// detectFilePaths returns the file paths in text, but only when the entire
// paste is paths — a paste mixing prose and paths must stay text, not
// silently become attachments.
func detectFilePaths(text, workingDir string) []string {
	lines := strings.Split(strings.TrimSpace(text), "\n")

	var paths []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Ignore comment lines in text/uri-list payloads (# ...) only when other
		// lines contain valid file URIs/paths.
		if strings.HasPrefix(line, "#") && len(lines) > 1 {
			continue
		}

		if resolved := resolveValidFile(line, workingDir); resolved != "" {
			paths = append(paths, resolved)
			continue
		}

		tokens := splitPastedLinePaths(line)
		if len(tokens) == 0 {
			return nil
		}
		for _, tok := range tokens {
			resolved := resolveValidFile(tok, workingDir)
			if resolved == "" {
				return nil
			}
			paths = append(paths, resolved)
		}
	}

	return paths
}

func resolveValidFile(candidate, workingDir string) string {
	candidate = strings.TrimSpace(candidate)
	if len(candidate) >= 2 {
		if (candidate[0] == '"' && candidate[len(candidate)-1] == '"') ||
			(candidate[0] == '\'' && candidate[len(candidate)-1] == '\'') {
			candidate = candidate[1 : len(candidate)-1]
		}
	}
	if uriPath, ok := fileuri.Path(candidate); ok {
		candidate = uriPath
	}
	if !isLikelyFilePath(candidate) {
		return ""
	}
	resolved := resolveFilePath(candidate, workingDir)
	if resolved == "" {
		return ""
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return ""
	}
	return resolved
}

// splitPastedLinePaths splits a single line containing one or more shell-escaped
// or quoted file paths (such as terminal drag-and-drop on macOS/Linux/Windows).
func splitPastedLinePaths(line string) []string {
	var tokens []string
	var cur strings.Builder
	var quote byte
	inToken := false

	for i := 0; i < len(line); i++ {
		ch := line[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else if ch == '\\' && quote == '"' && i+1 < len(line) && (line[i+1] == '"' || line[i+1] == '\\') {
				i++
				cur.WriteByte(line[i])
			} else {
				cur.WriteByte(ch)
			}
			continue
		}

		if ch == '\'' || ch == '"' {
			quote = ch
			inToken = true
			continue
		}

		if ch == ' ' || ch == '\t' {
			if inToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inToken = false
			}
			continue
		}

		// Terminals escape spaces and shell characters with backslashes on drag-and-drop.
		// Preserve Windows path separators by only treating backslash as an escape before
		// spaces or common shell metacharacters.
		if ch == '\\' && i+1 < len(line) && isShellEscapedPathChar(line[i+1]) {
			i++
			cur.WriteByte(line[i])
			inToken = true
			continue
		}

		cur.WriteByte(ch)
		inToken = true
	}

	if quote != 0 {
		return nil
	}
	if inToken {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func isShellEscapedPathChar(ch byte) bool {
	switch ch {
	case ' ', '\t', '(', ')', '[', ']', '&', ';', '!', '$', '\'', '"', '`', '#', '*', '?':
		return true
	default:
		return false
	}
}

func isLikelyFilePath(s string) bool {
	if strings.ContainsAny(s, "{}<>|") {
		return false
	}

	if !strings.Contains(s, "/") && !strings.Contains(s, "\\") {
		return false
	}

	if filepath.IsAbs(s) {
		return true
	}

	if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		return true
	}

	if strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, `.\`) || strings.HasPrefix(s, `..\`) {
		return true
	}

	return false
}

func resolveFilePath(path, workingDir string) string {
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}

		path = filepath.Join(home, path[2:])
	}

	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDir, path)
	}

	return filepath.Clean(path)
}

func normalizeFilePath(absPath, workingDir string) string {
	rel, err := filepath.Rel(workingDir, absPath)
	if err != nil {
		return absPath
	}

	if strings.HasPrefix(rel, "..") {
		return absPath
	}

	return rel
}

// normalizeClipboardImage validates and normalizes a clipboard image data URL
// (including resizing oversized screenshots) when it contains a real bitmap,
// while preserving synthetic test data URLs that fit within attachment limits.
func normalizeClipboardImage(dataURL string) string {
	rest, ok := strings.CutPrefix(dataURL, "data:")
	if !ok {
		return dataURL
	}
	_, encoded, ok := strings.Cut(rest, ";base64,")
	if !ok || encoded == "" {
		return dataURL
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || tool.ImageMIME(raw) == "" {
		return dataURL
	}
	res, err := tool.ReadImage(context.Background(), strings.NewReader(string(raw)))
	if err != nil || res.Content == "" {
		return dataURL
	}
	return res.Content
}

// readAttachedImageFile checks whether path is a supported raster image file
// and returns its normalized data URL via tool.ReadImage for direct vision input.
func readAttachedImageFile(path, workingDir string) string {
	resolved := resolveFilePath(path, workingDir)
	if resolved == "" {
		return ""
	}
	f, err := os.Open(resolved)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > tool.MaxImageSourceBytes {
		return ""
	}

	var head [512]byte
	n, err := f.Read(head[:])
	if err != nil && err != io.EOF {
		return ""
	}
	if tool.ImageMIME(head[:n]) == "" {
		return ""
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	res, err := tool.ReadImage(context.Background(), f)
	if err != nil {
		return ""
	}
	return res.Content
}
