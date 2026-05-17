package fs

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const (
	DefaultGrepLimit     = 250
	DefaultScanBufSize   = 64 * 1024
	MaxScanBufSize       = 1024 * 1024
	MaxLineDisplayLength = 500
)

// grepArgs is the parsed/validated form of the grep tool's input map.
type grepArgs struct {
	searchPath      string
	searchPathFS    string
	globPatterns    []string
	typeFilter      string
	multiline       bool
	showLineNumbers bool
	beforeContext   int
	afterContext    int
	headLimit       int
	unlimited       bool
	effectiveLimit  int
	resultOffset    int
	outputMode      string
	re              *regexp.Regexp
}

func parseGrepArgs(args map[string]any, workingDir string) (*grepArgs, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	searchPath := "."
	if p, ok := args["path"].(string); ok && p != "" {
		searchPath = p
	}
	searchPathFS, err := ensurePathInWorkspaceFS(searchPath, workingDir, "search")
	if err != nil {
		return nil, err
	}

	var globPatterns []string
	if g, ok := args["glob"].(string); ok {
		globPatterns = splitGrepGlobs(g)
	}
	for _, glob := range globPatterns {
		if _, err := doublestar.Match(strings.TrimPrefix(glob, "!"), ""); err != nil {
			return nil, fmt.Errorf("invalid glob pattern: %w", err)
		}
	}

	typeFilter := ""
	if t, ok := args["type"].(string); ok {
		typeFilter = strings.ToLower(strings.TrimSpace(t))
	}
	if typeFilter != "" && !validGrepType(typeFilter) {
		return nil, fmt.Errorf("unsupported type %q (supported: %s)", typeFilter, strings.Join(supportedGrepTypes(), ", "))
	}

	ignoreCase, _ := args["-i"].(bool)
	multiline, _ := args["multiline"].(bool)
	showLineNumbers := true
	if n, ok := args["-n"].(bool); ok {
		showLineNumbers = n
	}

	// context / -C set both sides; -B / -A then override one side.
	// Aliases are applied in declaration order so the later key wins.
	beforeContext, afterContext := 0, 0
	for _, key := range []string{"context", "-C"} {
		if v, present, err := tool.NonNegIntArg(args, key); present {
			if err != nil {
				return nil, err
			}
			beforeContext, afterContext = v, v
		}
	}
	for _, key := range []string{"before_context", "-B"} {
		if v, present, err := tool.NonNegIntArg(args, key); present {
			if err != nil {
				return nil, err
			}
			beforeContext = v
		}
	}
	for _, key := range []string{"after_context", "-A"} {
		if v, present, err := tool.NonNegIntArg(args, key); present {
			if err != nil {
				return nil, err
			}
			afterContext = v
		}
	}

	headLimit := DefaultGrepLimit
	if v, present, err := tool.NonNegIntArg(args, "head_limit"); present {
		if err != nil {
			return nil, err
		}
		headLimit = v
	}

	resultOffset := 0
	if v, present, err := tool.NonNegIntArg(args, "offset"); present {
		if err != nil {
			return nil, err
		}
		resultOffset = v
	}

	outputMode := "files_with_matches"
	if m, ok := args["output_mode"].(string); ok && m != "" {
		outputMode = m
	}
	if !validGrepOutputMode(outputMode) {
		return nil, fmt.Errorf("output_mode must be content, files_with_matches, or count")
	}

	regexPattern := pattern
	flags := ""
	if ignoreCase {
		flags += "i"
	}
	if multiline {
		flags += "s"
	}
	if flags != "" {
		regexPattern = "(?" + flags + ")" + regexPattern
	}
	re, err := regexp.Compile(regexPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	unlimited := headLimit == 0
	effectiveLimit := headLimit
	if unlimited {
		effectiveLimit = math.MaxInt
	}

	return &grepArgs{
		searchPath:      searchPath,
		searchPathFS:    searchPathFS,
		globPatterns:    globPatterns,
		typeFilter:      typeFilter,
		multiline:       multiline,
		showLineNumbers: showLineNumbers,
		beforeContext:   beforeContext,
		afterContext:    afterContext,
		headLimit:       headLimit,
		unlimited:       unlimited,
		effectiveLimit:  effectiveLimit,
		resultOffset:    resultOffset,
		outputMode:      outputMode,
		re:              re,
	}, nil
}

func GrepTool(root *os.Root) tool.Tool {
	return tool.Tool{
		Name:   "grep",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			fmt.Sprintf("Search file contents with regex using ripgrep-style semantics. Respects .gitignore. Default limit: %d results.", DefaultGrepLimit),
			"- Always use this for content search instead of shell `grep`/`rg` when possible.",
			"- Supports regex syntax such as `log.*Error` or `function\\s+\\w+`; escape literal braces (`interface\\{\\}`).",
			"- `output_mode`: `files_with_matches` (default), `content`, or `count`.",
			"- Filter with `glob` (e.g. `*.js`, `**/*.tsx`) or `type` (e.g. `js`, `py`, `rust`). Use `path` for a subtree or single file.",
			"- For open-ended searches requiring multiple rounds, use the `agent` tool.",
			"- `head_limit`/`offset` paginate results; `offset` is 0-based and `head_limit=0` is unlimited. Use `multiline=true` for cross-line patterns.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":        map[string]any{"type": "string", "description": "The regular expression pattern to search for in file contents."},
				"path":           map[string]any{"type": "string", "description": "Search root; defaults to workspace."},
				"glob":           map[string]any{"type": "string", "description": "Glob pattern to filter files (e.g. `*.js`, `*.{ts,tsx}`); maps to rg --glob."},
				"type":           map[string]any{"type": "string", "description": "File type filter (e.g. `go`, `ts`, `tsx`, `py`)."},
				"-i":             map[string]any{"type": "boolean", "description": "Case-insensitive search."},
				"-n":             map[string]any{"type": "boolean", "description": "Show line numbers in content output. Defaults to true."},
				"multiline":      map[string]any{"type": "boolean", "description": "Allow patterns to span newlines."},
				"context":        map[string]any{"type": "integer", "description": "Lines of context before and after each match."},
				"-C":             map[string]any{"type": "integer", "description": "Alias for context."},
				"before_context": map[string]any{"type": "integer", "description": "Lines before each match (overrides context)."},
				"-B":             map[string]any{"type": "integer", "description": "Alias for before_context."},
				"after_context":  map[string]any{"type": "integer", "description": "Lines after each match (overrides context)."},
				"-A":             map[string]any{"type": "integer", "description": "Alias for after_context."},
				"head_limit":     map[string]any{"type": "integer", "description": fmt.Sprintf("Max results (default %d, 0 for unlimited).", DefaultGrepLimit)},
				"offset":         map[string]any{"type": "integer", "description": "0-based number of result entries to skip before applying head_limit."},
				"output_mode": map[string]any{
					"type":        "string",
					"description": "files_with_matches | content | count.",
					"enum":        []string{"content", "files_with_matches", "count"},
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			workingDir := root.Name()
			cfg, err := parseGrepArgs(args, workingDir)
			if err != nil {
				return "", err
			}

			searchPath := cfg.searchPath
			searchPathFS := cfg.searchPathFS
			globPatterns := cfg.globPatterns
			typeFilter := cfg.typeFilter
			multiline := cfg.multiline
			showLineNumbers := cfg.showLineNumbers
			beforeContext, afterContext := cfg.beforeContext, cfg.afterContext
			headLimit := cfg.headLimit
			unlimited := cfg.unlimited
			effectiveLimit := cfg.effectiveLimit
			resultOffset := cfg.resultOffset
			outputMode := cfg.outputMode
			re := cfg.re

			info, err := root.Stat(searchPathFS)

			if err != nil {
				return "", pathError("stat path", searchPath, searchPathFS, workingDir, err)
			}

			fsys := root.FS()

			if !info.IsDir() {
				if typeFilter != "" && !matchesType(searchPathFS, typeFilter) {
					return "No matches found", nil
				}
				if !matchesGrepGlobs(globPatterns, searchPathFS, searchPathFS) {
					return "No matches found", nil
				}

				reportPath := filepath.FromSlash(searchPathFS)

				if outputMode == "count" {
					count := countFileMatches(fsys, searchPathFS, re, multiline)
					if count == 0 || resultOffset > 0 {
						return "No matches found", nil
					}
					occurrences := "occurrences"
					if count == 1 {
						occurrences = "occurrence"
					}
					return fmt.Sprintf("%s:%d\n\nFound %d total %s across 1 file.", reportPath, count, count, occurrences), nil
				}

				if outputMode == "files_with_matches" {
					matches := searchFileWithContext(fsys, searchPathFS, re, 0, 0, 1, multiline, true)
					if len(matches) == 0 {
						return "No files found", nil
					}
					if resultOffset > 0 {
						return "No files found", nil
					}
					return fmt.Sprintf("Found 1 file\n%s", reportPath), nil
				}

				searchLimit := effectiveLimit
				if !unlimited {
					searchLimit = resultOffset + headLimit + 1
				}
				matches := searchFileWithContext(fsys, searchPathFS, re, beforeContext, afterContext, searchLimit, multiline, showLineNumbers)

				if len(matches) == 0 {
					return "No matches found", nil
				}

				if resultOffset > 0 {
					if resultOffset >= len(matches) {
						return "No matches found", nil
					}
					matches = matches[resultOffset:]
				}
				resultLimitReached := false
				if !unlimited && len(matches) > headLimit {
					matches = matches[:headLimit]
					resultLimitReached = true
				}

				output := strings.Join(matches, "\n")
				if resultLimitReached || resultOffset > 0 {
					if notice := formatGrepPaginationNotice(resultLimitReached, headLimit, resultOffset); notice != "" {
						output += "\n\n[" + notice + "]"
					}
				}

				return output, nil
			}

			var results []string
			matchCount := 0
			limitReached := false

			type fileMatch struct {
				path    string
				count   int
				modTime time.Time
			}
			var fileMatches []fileMatch

			err = walkGrepFiles(ctx, fsys, searchPathFS, func(path, relPath string, d fs.DirEntry) error {
				if !matchesGrepGlobs(globPatterns, path, relPath) {
					return nil
				}

				if typeFilter != "" && !matchesType(path, typeFilter) {
					return nil
				}

				if isBinaryFile(path) {
					return nil
				}

				if outputMode == "files_with_matches" {
					matches := searchFileWithContext(fsys, path, re, 0, 0, 1, multiline, true)
					if len(matches) > 0 {
						fileMatches = append(fileMatches, fileMatch{path: filepath.FromSlash(path), modTime: entryModTime(d)})
					}
					return nil
				}

				if outputMode == "count" {
					count := countFileMatches(fsys, path, re, multiline)
					if count > 0 {
						fileMatches = append(fileMatches, fileMatch{path: filepath.FromSlash(path), count: count})
					}
					return nil
				}

				remaining := effectiveLimit
				if !unlimited {
					remaining = resultOffset + headLimit + 1 - matchCount
				}

				if !unlimited && remaining <= 0 {
					limitReached = true

					return filepath.SkipAll
				}

				matches := searchFileWithContext(fsys, path, re, beforeContext, afterContext, remaining, multiline, showLineNumbers)

				for _, m := range matches {
					matchCount++

					if matchCount <= resultOffset {
						continue
					}

					results = append(results, m)

					if !unlimited && len(results) > headLimit {
						limitReached = true
						return filepath.SkipAll
					}
				}

				return nil
			})

			if err != nil && err != filepath.SkipAll {
				return "", fmt.Errorf("search failed: %w", err)
			}

			var output string

			switch outputMode {
			case "files_with_matches":
				if len(fileMatches) == 0 {
					return "No files found", nil
				}
				// Newest mtime first; lexical path as a stable tiebreaker.
				slices.SortFunc(fileMatches, func(a, b fileMatch) int {
					return cmp.Or(b.modTime.Compare(a.modTime), cmp.Compare(a.path, b.path))
				})
				if resultOffset >= len(fileMatches) {
					return "No files found", nil
				}
				start := resultOffset
				end := len(fileMatches)
				if !unlimited && start+headLimit < end {
					end = start + headLimit
					limitReached = true
				}
				fileMatches = fileMatches[start:end]
				paths := make([]string, len(fileMatches))
				for i, fm := range fileMatches {
					paths[i] = fm.path
				}
				limitInfo := formatGrepLimitInfo(limitReached, headLimit, resultOffset)
				output = fmt.Sprintf("Found %d %s%s\n%s", len(paths), plural(len(paths), "file"), limitInfo, strings.Join(paths, "\n"))

			case "count":
				if len(fileMatches) == 0 {
					return "No matches found", nil
				}
				if resultOffset >= len(fileMatches) {
					return "No matches found", nil
				}
				start := resultOffset
				end := len(fileMatches)
				if !unlimited && start+headLimit < end {
					end = start + headLimit
					limitReached = true
				}
				fileMatches = fileMatches[start:end]
				lines := make([]string, len(fileMatches))
				totalMatches := 0
				for i, fm := range fileMatches {
					lines[i] = fmt.Sprintf("%s:%d", fm.path, fm.count)
					totalMatches += fm.count
				}
				output = strings.Join(lines, "\n")
				limitInfo := formatGrepLimitInfo(limitReached, headLimit, resultOffset)
				pagination := ""
				if limitInfo != "" {
					pagination = " with pagination =" + limitInfo
				}
				output += fmt.Sprintf("\n\nFound %d total %s across %d %s.%s", totalMatches, plural(totalMatches, "occurrence"), len(fileMatches), plural(len(fileMatches), "file"), pagination)

			default:
				if len(results) == 0 {
					return "No matches found", nil
				}
				if !unlimited && len(results) > headLimit {
					results = results[:headLimit]
					limitReached = true
				}
				output = strings.Join(results, "\n")
			}

			output, truncated := truncateHead(output)

			var notices []string

			if outputMode == "content" && (limitReached || resultOffset > 0) {
				if notice := formatGrepPaginationNotice(limitReached, headLimit, resultOffset); notice != "" {
					notices = append(notices, notice)
				}
			}

			if truncated {
				notices = append(notices, fmt.Sprintf("%dKB cap", DefaultMaxBytes/1024))
			}

			if len(notices) > 0 {
				output += "\n\n[" + strings.Join(notices, "; ") + "]"
			}

			return output, nil
		},
	}
}

func validGrepOutputMode(mode string) bool {
	switch mode {
	case "content", "files_with_matches", "count":
		return true
	default:
		return false
	}
}

func splitGrepGlobs(glob string) []string {
	var patterns []string
	for _, field := range strings.Fields(glob) {
		if strings.Contains(field, "{") && strings.Contains(field, "}") {
			patterns = append(patterns, field)
			continue
		}
		for _, part := range strings.Split(field, ",") {
			if part = strings.TrimSpace(part); part != "" {
				patterns = append(patterns, part)
			}
		}
	}
	return patterns
}

func matchesGrepGlobs(globs []string, path, relPath string) bool {
	if len(globs) == 0 {
		return true
	}

	matchedPositive := false
	hasPositive := false
	for _, glob := range globs {
		negated := strings.HasPrefix(glob, "!")
		pattern := strings.TrimPrefix(glob, "!")
		if pattern == "" {
			continue
		}

		matched := matchesSingleGrepGlob(pattern, path, relPath)
		if negated {
			if matched {
				return false
			}
			continue
		}

		hasPositive = true
		if matched {
			matchedPositive = true
		}
	}

	return !hasPositive || matchedPositive
}

func matchesSingleGrepGlob(pattern, path, relPath string) bool {
	if matched, _ := doublestar.Match(pattern, filepath.Base(path)); matched {
		return true
	}
	if matched, _ := doublestar.Match(pattern, filepath.ToSlash(relPath)); matched {
		return true
	}
	matched, _ := doublestar.Match(pattern, filepath.ToSlash(path))
	return matched
}

// binaryPeekSize is the prefix length sniffed for null bytes when deciding
// whether a file is binary. 512 bytes is enough to catch common formats
// (executables, images, compressed archives) without paying for a full read.
const binaryPeekSize = 512

// openTextFile opens path and returns a reader over the full contents.
// If the prefix contains a null byte the file is treated as binary,
// isBinary is set, the file is closed, and the reader is nil.
// Caller closes the returned io.ReadCloser on the non-binary path.
func openTextFile(fsys fs.FS, path string) (io.ReadCloser, bool, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, false, err
	}

	var peek [binaryPeekSize]byte
	n, err := io.ReadFull(f, peek[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		f.Close()
		return nil, false, err
	}

	if bytes.IndexByte(peek[:n], 0) != -1 {
		f.Close()
		return nil, true, nil
	}

	// Re-front the peeked bytes onto the file so the caller can scan from byte 0.
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek[:n]), f), f}, false, nil
}

func countFileMatches(fsys fs.FS, path string, re *regexp.Regexp, multiline bool) int {
	rc, isBinary, err := openTextFile(fsys, path)
	if err != nil || isBinary {
		return 0
	}
	defer rc.Close()

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, DefaultScanBufSize), MaxScanBufSize)

	// Multiline mode needs the joined buffer so the regex can span newlines;
	// keep a single builder rather than materializing each line into a slice.
	if multiline {
		var b strings.Builder
		first := true
		for scanner.Scan() {
			if !first {
				b.WriteByte('\n')
			}
			first = false
			b.Write(scanner.Bytes())
		}
		return len(re.FindAllStringIndex(b.String(), -1))
	}

	count := 0
	for scanner.Scan() {
		count += len(re.FindAllIndex(scanner.Bytes(), -1))
	}
	return count
}

var grepTypeExtensions = map[string][]string{
	"c":    {".c"},
	"cpp":  {".cpp", ".cc", ".cxx", ".c++", ".hpp", ".hh", ".hxx", ".h++"},
	"cs":   {".cs"},
	"css":  {".css"},
	"go":   {".go"},
	"h":    {".h", ".hpp", ".hh", ".hxx"},
	"html": {".html", ".htm"},
	"java": {".java"},
	"js":   {".js", ".jsx", ".mjs", ".cjs"},
	"json": {".json"},
	"md":   {".md", ".markdown"},
	"php":  {".php"},
	"py":   {".py", ".pyw"},
	"rb":   {".rb"},
	"rs":   {".rs"},
	"sh":   {".sh", ".bash", ".zsh"},
	"ts":   {".ts", ".mts", ".cts"},
	"tsx":  {".tsx"},
	"yaml": {".yaml", ".yml"},
	"yml":  {".yaml", ".yml"},
}

func validGrepType(typeFilter string) bool {
	_, ok := grepTypeExtensions[typeFilter]
	return ok
}

func supportedGrepTypes() []string {
	types := make([]string, 0, len(grepTypeExtensions))
	for t := range grepTypeExtensions {
		types = append(types, t)
	}
	slices.Sort(types)
	return types
}

func matchesType(path, typeFilter string) bool {
	exts, ok := grepTypeExtensions[typeFilter]
	if !ok {
		return false
	}
	return slices.Contains(exts, strings.ToLower(filepath.Ext(path)))
}

func plural(n int, singular string) string {
	if n == 1 {
		return singular
	}
	return singular + "s"
}

func formatGrepLimitInfo(limitReached bool, limit, offset int) string {
	var parts []string
	if limitReached {
		parts = append(parts, fmt.Sprintf("limit: %d", limit))
	}
	if offset > 0 {
		parts = append(parts, fmt.Sprintf("offset: %d", offset))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, ", ")
}

func formatGrepPaginationNotice(limitReached bool, limit, offset int) string {
	info := strings.TrimSpace(formatGrepLimitInfo(limitReached, limit, offset))
	if info == "" {
		return ""
	}
	return "Showing results with pagination = " + info
}

// lineContaining returns the index of the line whose start offset is the
// largest value <= offset, clamped to >= 0. lineStarts must be sorted.
func lineContaining(lineStarts []int, offset int) int {
	i, found := slices.BinarySearch(lineStarts, offset)
	if !found {
		i--
	}
	return max(0, i)
}

// entryModTime returns the directory entry's modification time, or zero if
// the underlying filesystem cannot report it.
func entryModTime(d fs.DirEntry) time.Time {
	info, err := d.Info()
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func walkGrepFiles(ctx context.Context, fsys fs.FS, root string, onFile func(path, relPath string, d fs.DirEntry) error) error {
	cache := newGitignoreCache(fsys)

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
			if cache.matches(path, true) {
				return filepath.SkipDir
			}
			return nil
		}

		if cache.matches(path, false) {
			return nil
		}

		return onFile(path, relPathFromBase(root, path), d)
	})
}

// gitignoreCache memoizes per-directory pattern lists so each directory's
// .gitignore is read at most once during a walk, instead of once per file.
type gitignoreCache struct {
	fsys     fs.FS
	patterns map[string][]gitignore.Pattern // key: directory fs path ("." or "a/b")
}

func newGitignoreCache(fsys fs.FS) *gitignoreCache {
	return &gitignoreCache{
		fsys:     fsys,
		patterns: make(map[string][]gitignore.Pattern),
	}
}

func (c *gitignoreCache) matches(path string, isDir bool) bool {
	dir := path
	if !isDir {
		dir = pathpkg.Dir(dir)
	}

	patterns := c.patternsFor(dir)
	if len(patterns) == 0 {
		return false
	}

	return gitignore.NewMatcher(patterns).Match(strings.Split(path, "/"), isDir)
}

func (c *gitignoreCache) patternsFor(dir string) []gitignore.Pattern {
	if cached, ok := c.patterns[dir]; ok {
		return cached
	}

	var parentPatterns []gitignore.Pattern
	if dir == "." || dir == "/" {
		parentPatterns = nil
	} else {
		parentPatterns = c.patternsFor(pathpkg.Dir(dir))
	}

	local := loadGitignore(c.fsys, pathDomain(dir))
	if len(local) == 0 {
		c.patterns[dir] = parentPatterns
		return parentPatterns
	}

	combined := make([]gitignore.Pattern, 0, len(parentPatterns)+len(local))
	combined = append(combined, parentPatterns...)
	combined = append(combined, local...)
	c.patterns[dir] = combined
	return combined
}

func searchFileWithContext(fsys fs.FS, path string, re *regexp.Regexp, beforeContext, afterContext, limit int, multiline bool, showLineNumbers bool) []string {
	rc, isBinary, err := openTextFile(fsys, path)
	if err != nil || isBinary {
		return nil
	}
	defer rc.Close()

	displayPath := filepath.FromSlash(path)

	var lines []string
	scanner := bufio.NewScanner(rc)

	buf := make([]byte, 0, DefaultScanBufSize)
	scanner.Buffer(buf, MaxScanBufSize)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	// Preserve `lines` collected before the error. A line longer than
	// MaxScanBufSize (1MB) — common in minified bundles or generated JSON —
	// otherwise drops the entire file from results, masking real matches in
	// the preceding lines. Emit a sentinel `match` for the file so callers
	// (and the model) can tell scanning stopped early.
	var scanCutoff string
	if err := scanner.Err(); err != nil {
		if !errors.Is(err, bufio.ErrTooLong) {
			// Other scanner errors (I/O) — bail like before.
			return nil
		}
		scanCutoff = formatGrepLine(displayPath, len(lines)+1, true, fmt.Sprintf("line exceeds %dKB scan limit; remainder of file skipped", MaxScanBufSize/1024), showLineNumbers)
	}

	var results []string
	matchedLines := make([]bool, len(lines))
	anyMatch := false

	if multiline {
		// In multiline mode the regex may span newlines, so we must run it
		// against the joined file content rather than line-by-line. Each match
		// is then mapped back to the line range it covers, and every line in
		// that range is marked as matched so context formatting still works.
		full := strings.Join(lines, "\n")

		// lineStarts[i] = byte offset of the start of line i in `full`.
		lineStarts := make([]int, len(lines))
		offset := 0
		for i, line := range lines {
			lineStarts[i] = offset
			offset += len(line) + 1
		}

		for _, m := range re.FindAllStringIndex(full, -1) {
			start, end := m[0], m[1]
			startLine := lineContaining(lineStarts, start)
			// For zero-width matches, end equals start; clamp so we still mark the starting line.
			endProbe := end
			if endProbe > start {
				endProbe = end - 1
			}
			endLine := max(startLine, lineContaining(lineStarts, endProbe))
			for i := startLine; i <= endLine; i++ {
				matchedLines[i] = true
				anyMatch = true
			}
		}
	} else {
		for i, line := range lines {
			if re.MatchString(line) {
				matchedLines[i] = true
				anyMatch = true
			}
		}
	}

	if !anyMatch {
		return nil
	}

	printed := make([]bool, len(lines))
	lastPrinted := -2

	for i := range lines {
		if !matchedLines[i] {
			continue
		}

		if len(results) >= limit {
			break
		}

		start := max(0, i-beforeContext)
		end := min(len(lines)-1, i+afterContext)

		if lastPrinted >= 0 && start > lastPrinted+1 {
			results = append(results, "--")
		}

		for j := start; j <= end; j++ {
			if printed[j] {
				continue
			}
			printed[j] = true

			lineContent := lines[j]

			if len(lineContent) > MaxLineDisplayLength {
				lineContent = lineContent[:MaxLineDisplayLength-3] + "..."
			}

			results = append(results, formatGrepLine(displayPath, j+1, matchedLines[j], lineContent, showLineNumbers))
			lastPrinted = j
		}
	}

	if scanCutoff != "" && len(results) < limit {
		results = append(results, scanCutoff)
	}

	return results
}

func formatGrepLine(path string, lineNumber int, matched bool, content string, showLineNumbers bool) string {
	separator := "-"
	if matched {
		separator = ":"
	}
	if showLineNumbers {
		return fmt.Sprintf("%s%s%d%s%s", path, separator, lineNumber, separator, content)
	}
	return fmt.Sprintf("%s%s%s", path, separator, content)
}
