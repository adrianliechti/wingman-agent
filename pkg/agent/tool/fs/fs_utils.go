package fs

import (
	"bufio"
	"fmt"
	"io/fs"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
	DefaultMaxLines = 2000
	DefaultMaxBytes = 30 * 1024

	// MaxEditFileBytes caps the file size that `edit` will operate on.
	// Larger files should be rewritten with `write` rather than patched.
	MaxEditFileBytes = 10 * 1024 * 1024
)

// normalizePath converts an absolute path to a relative path if it starts with the working directory.
// This is needed because os.Root expects relative paths, but the LLM may provide absolute paths.
// Always returns paths with OS-native separators (backslash on Windows, forward slash on Unix).
func normalizePath(path, workingDir string) string {
	if !filepath.IsAbs(path) {
		return filepath.FromSlash(path)
	}

	if rel, ok := relPathWithinWorkspace(path, workingDir); ok {
		return rel
	}

	return filepath.FromSlash(path)
}

func normalizePathFS(path, workingDir string) string {
	return pathpkg.Clean(filepath.ToSlash(normalizePath(path, workingDir)))
}

func ensurePathInWorkspace(pathArg, workingDir, action string) (string, error) {
	if isOutsideWorkspace(pathArg, workingDir) {
		return "", fmt.Errorf("cannot %s: path %q is outside workspace %q", action, pathArg, workingDir)
	}

	return normalizePath(pathArg, workingDir), nil
}

func ensurePathInWorkspaceFS(pathArg, workingDir, action string) (string, error) {
	if isOutsideWorkspace(pathArg, workingDir) {
		return "", fmt.Errorf("cannot %s: path %q is outside workspace %q", action, pathArg, workingDir)
	}

	return normalizePathFS(pathArg, workingDir), nil
}

// writeTarget classifies a write/edit path. Exactly one of (InWorkspace,
// AbsoluteAllowed) is true on success. RelPath is set when InWorkspace
// (root-relative for os.Root ops); AbsPath is set when AbsoluteAllowed
// (raw os.* ops). Mirrors the read-side classification in
// readFromAllowedLocation but returns a target descriptor instead of
// performing the read inline.
type writeTarget struct {
	InWorkspace     bool
	AbsoluteAllowed bool
	RelPath         string
	AbsPath         string
}

// resolveWriteTarget classifies pathArg for write/edit. Paths inside
// workingDir resolve to root-relative form. Absolute paths inside any
// allowedWriteRoot resolve to absolute form for raw os.* ops. Anything
// else is rejected.
func resolveWriteTarget(pathArg, workingDir string, allowedWriteRoots []string, action string) (writeTarget, error) {
	if !isOutsideWorkspace(pathArg, workingDir) {
		return writeTarget{InWorkspace: true, RelPath: normalizePath(pathArg, workingDir)}, nil
	}

	if !filepath.IsAbs(pathArg) {
		return writeTarget{}, fmt.Errorf("cannot %s: relative path %q is outside workspace", action, pathArg)
	}

	cleaned := cleanPath(pathArg)
	cmpPath := normalizePathForComparison(cleaned)

	for _, allowed := range allowedWriteRoots {
		allowedClean := cleanPath(allowed)
		cmpAllowed := normalizePathForComparison(allowedClean)
		if cmpPath == cmpAllowed || strings.HasPrefix(cmpPath, cmpAllowed+string(filepath.Separator)) {
			return writeTarget{AbsoluteAllowed: true, AbsPath: cleaned}, nil
		}
	}

	return writeTarget{}, fmt.Errorf("cannot %s: path %q is outside workspace %q and not in any allowed write root", action, pathArg, workingDir)
}

func isOutsideWorkspace(path, workingDir string) bool {
	if !filepath.IsAbs(path) {
		return false
	}

	_, ok := relPathWithinWorkspace(path, workingDir)

	return !ok
}

// relPathWithinWorkspace returns the relative path from workingDir to absPath
// if absPath is within workingDir. It preserves the original casing where possible.
func relPathWithinWorkspace(absPath, workingDir string) (string, bool) {
	if !filepath.IsAbs(absPath) {
		return filepath.FromSlash(absPath), true
	}

	absPathClean := cleanPath(absPath)
	absWorkingDir := cleanPath(workingDir)

	compPath := normalizePathForComparison(absPathClean)
	compWorking := normalizePathForComparison(absWorkingDir)
	sep := string(filepath.Separator)

	if compPath == compWorking {
		return ".", true
	}

	prefix := compWorking
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}

	if strings.HasPrefix(compPath, prefix) {
		if strings.HasSuffix(absWorkingDir, sep) {
			return absPathClean[len(absWorkingDir):], true
		}

		return absPathClean[len(absWorkingDir)+len(sep):], true
	}

	relComp, err := filepath.Rel(compWorking, compPath)

	if err != nil {
		return "", false
	}

	if relComp == "." {
		return ".", true
	}

	if relComp == ".." || strings.HasPrefix(relComp, ".."+sep) {
		return "", false
	}

	if relOrig, err := filepath.Rel(absWorkingDir, absPathClean); err == nil {
		if relOrig == "." {
			return ".", true
		}

		if relOrig != ".." && !strings.HasPrefix(relOrig, ".."+sep) {
			return relOrig, true
		}
	}

	return relComp, true
}

func cleanPath(path string) string {
	if path == "" {
		return path
	}

	return filepath.Clean(filepath.FromSlash(path))
}

// normalizePathForComparison normalizes paths for case-insensitive comparison.
// Windows paths are fully case-insensitive, and macOS (APFS) is case-insensitive by default.
// We treat both as case-insensitive for path comparison.
func normalizePathForComparison(path string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(path)
	}
	return path
}

func pathError(action, originalPath, normalizedPath, workingDir string, err error) error {
	if isOutsideWorkspace(originalPath, workingDir) {
		return fmt.Errorf("%s failed: path %q is outside workspace %q", action, originalPath, workingDir)
	}

	if originalPath != normalizedPath {
		return fmt.Errorf("%s failed: %s (resolved from %s): %w", action, normalizedPath, originalPath, err)
	}

	return fmt.Errorf("%s failed: %s: %w", action, originalPath, err)
}

func detectLineEnding(content string) string {
	if strings.Contains(content, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

func normalizeToLF(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	return text
}

func restoreLineEndings(text, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}

	return text
}

func stripBom(content string) (bom string, text string) {
	if strings.HasPrefix(content, "\uFEFF") {
		return "\uFEFF", content[len("\uFEFF"):]
	}

	return "", content
}

func normalizeForFuzzyMatch(text string) string {
	normalized, _ := normalizeForFuzzyMatchWithMap(text)
	return normalized
}

func normalizeForFuzzyMatchWithMap(text string) (string, []int) {
	var b strings.Builder
	offsetMap := []int{0}

	for lineStart := 0; lineStart <= len(text); {
		lineEnd := strings.IndexByte(text[lineStart:], '\n')
		hasNewline := lineEnd != -1
		if hasNewline {
			lineEnd += lineStart
		} else {
			lineEnd = len(text)
		}

		trimmedEnd := lineEnd
		for trimmedEnd > lineStart && (text[trimmedEnd-1] == ' ' || text[trimmedEnd-1] == '\t') {
			trimmedEnd--
		}

		for pos := lineStart; pos < trimmedEnd; {
			r, size := utf8.DecodeRuneInString(text[pos:trimmedEnd])
			replacement := normalizeFuzzyRune(r)
			b.WriteString(replacement)

			originalBytes := text[pos : pos+size]
			if replacement == originalBytes {
				for i := 1; i <= size; i++ {
					offsetMap = append(offsetMap, pos+i)
				}
			} else {
				for i := 0; i < len(replacement); i++ {
					offsetMap = append(offsetMap, pos+size)
				}
			}

			pos += size
		}

		if !hasNewline {
			break
		}

		b.WriteByte('\n')
		offsetMap = append(offsetMap, lineEnd+1)
		lineStart = lineEnd + 1
	}

	return b.String(), offsetMap
}

func normalizeFuzzyRune(r rune) string {
	switch r {
	case '\u2018', '\u2019', '\u201A', '\u201B':
		return "'"
	case '\u201C', '\u201D', '\u201E', '\u201F':
		return "\""
	case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
		return "-"
	case '\u00A0', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008',
		'\u2009', '\u200A', '\u202F', '\u205F', '\u3000':
		return " "
	default:
		return string(r)
	}
}

type fuzzyMatchResult struct {
	found          bool
	index          int
	matchLength    int
	usedFuzzyMatch bool
}

func fuzzyFindText(content, oldText string) fuzzyMatchResult {
	if i := strings.Index(content, oldText); i != -1 {
		return fuzzyMatchResult{found: true, index: i, matchLength: len(oldText)}
	}

	fuzzyOldText := normalizeForFuzzyMatch(oldText)
	if fuzzyOldText == "" {
		return fuzzyMatchResult{index: -1}
	}

	fuzzyContent, fuzzyToOriginal := normalizeForFuzzyMatchWithMap(content)
	fuzzyIndex := strings.Index(fuzzyContent, fuzzyOldText)
	if fuzzyIndex == -1 {
		return fuzzyMatchResult{index: -1}
	}

	originalIndex := fuzzyToOriginal[fuzzyIndex]
	originalEnd := fuzzyToOriginal[fuzzyIndex+len(fuzzyOldText)]

	return fuzzyMatchResult{
		found:          true,
		index:          originalIndex,
		matchLength:    originalEnd - originalIndex,
		usedFuzzyMatch: true,
	}
}

func generateDiffString(oldContent, newContent string) string {
	dmp := diffmatchpatch.New()

	oldLines, newLines, lineArray := dmp.DiffLinesToChars(oldContent, newContent)
	diffs := dmp.DiffMain(oldLines, newLines, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)
	diffs = dmp.DiffCleanupSemantic(diffs)

	var output strings.Builder
	oldLineNum := 1
	newLineNum := 1

	for _, diff := range diffs {
		lines := strings.Split(diff.Text, "\n")

		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}

		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			oldLineNum += len(lines)
			newLineNum += len(lines)
		case diffmatchpatch.DiffDelete:
			for _, line := range lines {
				fmt.Fprintf(&output, "-%d %s\n", oldLineNum, line)
				oldLineNum++
			}
		case diffmatchpatch.DiffInsert:
			for _, line := range lines {
				fmt.Fprintf(&output, "+%d %s\n", newLineNum, line)
				newLineNum++
			}
		}
	}

	return output.String()
}

var vcsDirs = map[string]bool{
	".git": true,
	".svn": true,
	".hg":  true,
	".bzr": true,
	".jj":  true,
	".sl":  true,
}

var binaryExtensions = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".bin": true, ".dat": true, ".db": true, ".sqlite": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".bmp": true, ".ico": true, ".webp": true,
	".pdf": true, ".doc": true, ".docx": true, ".xls": true,
	".xlsx": true, ".ppt": true, ".pptx": true,
	".zip": true, ".tar": true, ".gz": true, ".rar": true,
	".7z": true, ".bz2": true, ".xz": true,
	".mp3": true, ".mp4": true, ".avi": true, ".mov": true,
	".wav": true, ".flac": true, ".ogg": true, ".webm": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".pyc": true, ".pyo": true, ".class": true, ".o": true, ".a": true,
}

func isBinaryFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))

	return binaryExtensions[ext]
}

func relPathSlash(base, target string) string {
	rel, err := filepath.Rel(filepath.FromSlash(base), filepath.FromSlash(target))

	if err != nil {
		return target
	}

	return filepath.ToSlash(rel)
}

func relPathFromBase(base, path string) string {
	if base == "." {
		return path
	}

	return relPathSlash(base, path)
}

func pathDomain(fsPath string) []string {
	if fsPath == "" || fsPath == "." {
		return nil
	}

	return strings.Split(fsPath, "/")
}

func loadGitignore(fsys fs.FS, domain []string) []gitignore.Pattern {
	gitignorePath := ".gitignore"

	if len(domain) > 0 {
		gitignorePath = pathpkg.Join(append(domain, ".gitignore")...)
	}

	f, err := fsys.Open(gitignorePath)

	if err != nil {
		return nil
	}
	defer f.Close()

	var patterns []gitignore.Pattern
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		patterns = append(patterns, gitignore.ParsePattern(line, domain))
	}

	return patterns
}
