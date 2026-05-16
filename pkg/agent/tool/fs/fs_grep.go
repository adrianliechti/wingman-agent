package fs

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const (
	DefaultGrepLimit     = 200
	DefaultScanBufSize   = 64 * 1024
	MaxScanBufSize       = 1024 * 1024
	MaxLineDisplayLength = 200
)

func GrepTool(root *os.Root) tool.Tool {
	return tool.Tool{
		Name:   "grep",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			fmt.Sprintf("Primary tool for code/content search. Search file contents for a regex pattern. Respects .gitignore. Default limit %d matches.", DefaultGrepLimit),
			"- Use this before `ls` or broad `read` when looking for symbols, functions, classes, config keys, TODOs, errors, text snippets, or files likely containing a term.",
			"- Regex by default; `literal=true` for exact strings or text with regex metacharacters. Literal braces need escaping (`interface\\{\\}`).",
			"- `output_mode`: \"files_with_matches\" (default — cheap, returns paths only), \"content\" (matched lines, optionally with `before_context`/`after_context`), \"count\". Start with files_with_matches; switch to content only when you need to see lines.",
			"- Narrow by `type` (e.g. `go`, `ts`, `py`) or `glob` (e.g. `**/*.go`, `*.{ts,tsx}`). Both apply when set. Use `path` to limit search to a subtree or single file.",
			"- Token efficiency: keep `head_limit` small until you know you need more; use `count` to size broad searches before asking for content.",
			"- `head_limit`/`offset` paginate result entries across all modes; `head_limit=0` is unlimited. `multiline=true` lets patterns span lines.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":          map[string]any{"type": "string", "description": "Regex pattern (or literal if literal=true)."},
				"path":             map[string]any{"type": "string", "description": "Search root; defaults to workspace."},
				"glob":             map[string]any{"type": "string", "description": "Filename filter (e.g. `*.go`, `*.{ts,tsx}`)."},
				"type":             map[string]any{"type": "string", "description": "File type filter (e.g. `go`, `ts`, `tsx`, `py`)."},
				"case_insensitive": map[string]any{"type": "boolean", "description": "Case-insensitive."},
				"-i":               map[string]any{"type": "boolean", "description": "Alias for case_insensitive."},
				"literal":          map[string]any{"type": "boolean", "description": "Treat pattern as literal string."},
				"multiline":        map[string]any{"type": "boolean", "description": "Allow patterns to span newlines."},
				"context":          map[string]any{"type": "integer", "description": "Lines of context before and after each match."},
				"-C":               map[string]any{"type": "integer", "description": "Alias for context."},
				"before_context":   map[string]any{"type": "integer", "description": "Lines before each match (overrides context)."},
				"-B":               map[string]any{"type": "integer", "description": "Alias for before_context."},
				"after_context":    map[string]any{"type": "integer", "description": "Lines after each match (overrides context)."},
				"-A":               map[string]any{"type": "integer", "description": "Alias for after_context."},
				"head_limit":       map[string]any{"type": "integer", "description": fmt.Sprintf("Max results (default %d, 0 for unlimited).", DefaultGrepLimit)},
				"offset":           map[string]any{"type": "integer", "description": "0-based number of result entries to skip before applying head_limit."},
				"output_mode": map[string]any{
					"type":        "string",
					"description": "files_with_matches | content | count.",
					"enum":        []string{"content", "files_with_matches", "count"},
				},
			},
			"required": []string{"pattern"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pattern, ok := args["pattern"].(string)

			if !ok || pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}

			searchPath := "."

			if p, ok := args["path"].(string); ok && p != "" {
				searchPath = p
			}

			workingDir := root.Name()

			searchPathFS, err := ensurePathInWorkspaceFS(searchPath, workingDir, "search")

			if err != nil {
				return "", err
			}

			glob := ""

			if g, ok := args["glob"].(string); ok {
				glob = g
			}

			typeFilter := ""

			if t, ok := args["type"].(string); ok {
				typeFilter = strings.ToLower(strings.TrimSpace(t))
			}
			if typeFilter != "" && !validGrepType(typeFilter) {
				return "", fmt.Errorf("unsupported type %q (supported: %s)", typeFilter, strings.Join(supportedGrepTypes(), ", "))
			}

			ignoreCase := false

			if ic, ok := args["case_insensitive"].(bool); ok {
				ignoreCase = ic
			}

			if ic, ok := args["-i"].(bool); ok {
				ignoreCase = ic
			}

			multiline := false

			if ml, ok := args["multiline"].(bool); ok {
				multiline = ml
			}

			contextLines := 0
			beforeContext := 0
			afterContext := 0

			if c, ok := optionalInt(args, "context"); ok && c > 0 {
				contextLines = c
				beforeContext = contextLines
				afterContext = contextLines
			}

			if c, ok := optionalInt(args, "-C"); ok && c > 0 {
				contextLines = c
				beforeContext = contextLines
				afterContext = contextLines
			}

			if bc, ok := optionalInt(args, "before_context"); ok && bc > 0 {
				beforeContext = bc
			}

			if bc, ok := optionalInt(args, "-B"); ok && bc > 0 {
				beforeContext = bc
			}

			if ac, ok := optionalInt(args, "after_context"); ok && ac > 0 {
				afterContext = ac
			}

			if ac, ok := optionalInt(args, "-A"); ok && ac > 0 {
				afterContext = ac
			}

			headLimit := nonNegativeIntArg(args, "head_limit", DefaultGrepLimit)

			unlimited := headLimit == 0
			effectiveLimit := headLimit
			if unlimited {
				effectiveLimit = maxInt()
			}

			resultOffset := 0

			resultOffset = positiveIntArg(args, "offset", 0)

			literal := false

			if l, ok := args["literal"].(bool); ok {
				literal = l
			}

			outputMode := "files_with_matches"

			if m, ok := args["output_mode"].(string); ok && m != "" {
				outputMode = m
			}

			if !validGrepOutputMode(outputMode) {
				return "", fmt.Errorf("output_mode must be content, files_with_matches, or count")
			}

			regexPattern := pattern
			if literal {
				regexPattern = regexp.QuoteMeta(pattern)
			}

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
				return "", fmt.Errorf("invalid regex pattern: %w", err)
			}

			info, err := root.Stat(searchPathFS)

			if err != nil {
				return "", pathError("stat path", searchPath, searchPathFS, workingDir, err)
			}

			fsys := root.FS()

			if !info.IsDir() {
				if typeFilter != "" && !matchesType(searchPathFS, typeFilter) {
					return "No matches found", nil
				}
				if glob != "" {
					matched, err := doublestar.Match(glob, filepath.ToSlash(filepath.Base(searchPathFS)))
					if err != nil {
						return "", fmt.Errorf("invalid glob pattern: %w", err)
					}
					matchedPath, err := doublestar.Match(glob, filepath.ToSlash(searchPathFS))
					if err != nil {
						return "", fmt.Errorf("invalid glob pattern: %w", err)
					}
					if !matched && !matchedPath {
						return "No matches found", nil
					}
				}

				reportPath := searchPath

				if outputMode == "files_with_matches" && resultOffset > 0 {
					return "No matches found (offset beyond results)", nil
				}

				if outputMode == "count" {
					count := countFileMatches(fsys, searchPathFS, re, multiline)
					if count == 0 || resultOffset > 0 {
						return "No matches found", nil
					}
					return fmt.Sprintf("%s:%d", reportPath, count), nil
				}

				searchLimit := effectiveLimit
				if !unlimited {
					searchLimit += resultOffset
				}
				matches := searchFileWithContext(fsys, searchPathFS, re, beforeContext, afterContext, searchLimit, multiline)

				if len(matches) == 0 {
					return "No matches found", nil
				}

				if resultOffset > 0 {
					if resultOffset >= len(matches) {
						return "No matches found (offset beyond results)", nil
					}
					matches = matches[resultOffset:]
				}
				resultLimitReached := false
				if !unlimited && len(matches) > headLimit {
					matches = matches[:headLimit]
					resultLimitReached = true
				}

				if outputMode == "files_with_matches" {
					return reportPath, nil
				}

				output := strings.Join(matches, "\n")
				if resultLimitReached {
					output += fmt.Sprintf("\n\n[limit %d hit; offset=%d for more]", headLimit, resultOffset+headLimit)
				}

				return output, nil
			}

			var results []string
			matchCount := 0
			skippedCount := 0
			limitReached := false

			type fileMatch struct {
				path  string
				count int
			}
			var fileMatches []fileMatch

			err = walkWorkspace(ctx, fsys, searchPathFS, func(path, relPath string) error {
				if glob != "" {
					matched, _ := doublestar.Match(glob, pathpkg.Base(path))

					if !matched {
						matched, _ = doublestar.Match(glob, relPath)

						if !matched {
							return nil
						}
					}
				}

				if typeFilter != "" && !matchesType(path, typeFilter) {
					return nil
				}

				if isBinaryFile(path) {
					return nil
				}

				if outputMode == "files_with_matches" {
					matches := searchFileWithContext(fsys, path, re, 0, 0, 1, multiline)
					if len(matches) > 0 {
						matchCount++

						if matchCount <= resultOffset {
							skippedCount++
							return nil
						}

						fileMatches = append(fileMatches, fileMatch{path: filepath.FromSlash(relPath)})

						if !unlimited && len(fileMatches) >= headLimit {
							limitReached = true
							return filepath.SkipAll
						}
					}
					return nil
				}

				if outputMode == "count" {
					count := countFileMatches(fsys, path, re, multiline)
					if count > 0 {
						matchCount++

						if matchCount <= resultOffset {
							skippedCount++
							return nil
						}

						fileMatches = append(fileMatches, fileMatch{path: filepath.FromSlash(relPath), count: count})

						if !unlimited && len(fileMatches) >= headLimit {
							limitReached = true
							return filepath.SkipAll
						}
					}
					return nil
				}

				remaining := effectiveLimit - len(results) + resultOffset - skippedCount

				if !unlimited && remaining <= 0 {
					limitReached = true

					return filepath.SkipAll
				}

				matches := searchFileWithContext(fsys, path, re, beforeContext, afterContext, remaining, multiline)

				for _, m := range matches {
					matchCount++

					if matchCount <= resultOffset {
						skippedCount++
						continue
					}

					results = append(results, m)

					if !unlimited && len(results) >= headLimit {
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
					return "No matches found", nil
				}
				paths := make([]string, len(fileMatches))
				for i, fm := range fileMatches {
					paths[i] = fm.path
				}
				output = strings.Join(paths, "\n")

			case "count":
				if len(fileMatches) == 0 {
					return "No matches found", nil
				}
				lines := make([]string, len(fileMatches))
				for i, fm := range fileMatches {
					lines[i] = fmt.Sprintf("%s:%d", fm.path, fm.count)
				}
				output = strings.Join(lines, "\n")

			default:
				if len(results) == 0 {
					return "No matches found", nil
				}
				output = strings.Join(results, "\n")
			}

			output, truncated := truncateHead(output)

			var notices []string

			if limitReached {
				notices = append(notices, fmt.Sprintf("limit %d hit; offset=%d for more", headLimit, resultOffset+headLimit))
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

func countFileMatches(fsys fs.FS, path string, re *regexp.Regexp, multiline bool) int {
	f, err := fsys.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, DefaultScanBufSize)
	scanner.Buffer(buf, MaxScanBufSize)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if multiline {
		return len(re.FindAllStringIndex(strings.Join(lines, "\n"), -1))
	}

	count := 0
	for _, line := range lines {
		if re.MatchString(line) {
			count++
		}
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
	sort.Strings(types)
	return types
}

func matchesType(path, typeFilter string) bool {
	exts, ok := grepTypeExtensions[typeFilter]
	if !ok {
		return false
	}

	ext := strings.ToLower(filepath.Ext(path))
	for _, allowed := range exts {
		if ext == allowed {
			return true
		}
	}

	return false
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func searchFileWithContext(fsys fs.FS, path string, re *regexp.Regexp, beforeContext, afterContext, limit int, multiline bool) []string {
	f, err := fsys.Open(path)

	if err != nil {
		return nil
	}
	defer f.Close()

	displayPath := filepath.FromSlash(path)

	var lines []string
	scanner := bufio.NewScanner(f)

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
		if err == bufio.ErrTooLong {
			scanCutoff = fmt.Sprintf("%s:%d:! line exceeds %dKB scan limit; remainder of file skipped", displayPath, len(lines)+1, MaxScanBufSize/1024)
		} else {
			// Other scanner errors (I/O) — bail like before.
			return nil
		}
	}

	var results []string
	matchedLines := make(map[int]bool)

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
			startLine := max(0, sort.Search(len(lineStarts), func(i int) bool { return lineStarts[i] > start })-1)
			// For zero-width matches, end equals start; clamp so we still mark the starting line.
			endProbe := end
			if endProbe > start {
				endProbe = end - 1
			}
			endLine := max(startLine, sort.Search(len(lineStarts), func(i int) bool { return lineStarts[i] > endProbe })-1)
			for i := startLine; i <= endLine; i++ {
				matchedLines[i] = true
			}
		}
	} else {
		for i, line := range lines {
			if re.MatchString(line) {
				matchedLines[i] = true
			}
		}
	}

	if len(matchedLines) == 0 {
		if scanCutoff != "" {
			return []string{scanCutoff}
		}
		return nil
	}

	printed := make(map[int]bool)
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

			prefix := " "

			if matchedLines[j] {
				prefix = ">"
			}

			lineContent := lines[j]

			if len(lineContent) > MaxLineDisplayLength {
				lineContent = lineContent[:MaxLineDisplayLength-3] + "..."
			}

			results = append(results, fmt.Sprintf("%s:%d:%s %s", displayPath, j+1, prefix, lineContent))
			lastPrinted = j
		}
	}

	if scanCutoff != "" && len(results) < limit {
		results = append(results, scanCutoff)
	}

	return results
}
