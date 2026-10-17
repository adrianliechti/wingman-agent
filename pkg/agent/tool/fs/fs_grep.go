package fs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

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

type grepArgs struct {
	searchPath      string
	globPatterns    []string
	typeFilter      string
	multiline       bool
	showLineNumbers bool
	beforeContext   int
	afterContext    int
	headLimit       int
	effectiveLimit  int
	resultOffset    int
	outputMode      string
	re              *regexp.Regexp
}

func parseGrepArgs(args map[string]any) (*grepArgs, error) {
	for _, key := range []string{"path", "glob", "type", "output_mode"} {
		if value, exists := args[key]; exists && value != nil {
			if _, ok := value.(string); !ok {
				return nil, fmt.Errorf("%s must be a string", key)
			}
		}
	}
	for _, key := range []string{"-i", "-n", "multiline"} {
		if value, exists := args[key]; exists && value != nil {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%s must be a boolean", key)
			}
		}
	}
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	searchPath := "."
	if p, ok := args["path"].(string); ok && p != "" {
		searchPath = p
	}

	var globPatterns []string
	if g, ok := args["glob"].(string); ok {
		globPatterns = splitGrepGlobs(g)
	}
	for _, glob := range globPatterns {
		if glob == "!" {
			return nil, fmt.Errorf("invalid glob pattern: missing pattern after !")
		}
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

	beforeContext, afterContext := 0, 0
	for _, key := range []string{"context", "-C"} {
		if v, present, err := tool.NonNegIntArg(args, key); present {
			if err != nil {
				return nil, err
			}
			beforeContext, afterContext = v, v
		}
	}
	if v, present, err := tool.NonNegIntArg(args, "-B"); present {
		if err != nil {
			return nil, err
		}
		beforeContext = v
	}
	if v, present, err := tool.NonNegIntArg(args, "-A"); present {
		if err != nil {
			return nil, err
		}
		afterContext = v
	}

	headLimit := DefaultGrepLimit
	if v, present, err := tool.NonNegIntArg(args, "head_limit"); present {
		if err != nil {
			return nil, err
		}
		headLimit = v
	}

	resultOffset := 0
	for _, key := range []string{"skip", "offset"} {
		if v, present, err := tool.NonNegIntArg(args, key); present {
			if err != nil {
				return nil, err
			}
			resultOffset = v
			break
		}
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
		globPatterns:    globPatterns,
		typeFilter:      typeFilter,
		multiline:       multiline,
		showLineNumbers: showLineNumbers,
		beforeContext:   beforeContext,
		afterContext:    afterContext,
		headLimit:       headLimit,
		effectiveLimit:  effectiveLimit,
		resultOffset:    resultOffset,
		outputMode:      outputMode,
		re:              re,
	}, nil
}

func GrepTool(root *os.Root, allowedReadRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "grep",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			"Search file contents with regular expressions and optional context.",
			"- Supports regex syntax (e.g. `log.*Error`, `function\\s+\\w+`).",
			"- Pattern syntax is Go RE2 (not POSIX grep or PCRE): no lookaheads/lookbehinds or backreferences. Escape literal braces (use `interface\\{\\}` to find `interface{}`).",
			"- Filter files with `glob` (e.g. `*.js`, `*.{ts,tsx}`) or `type` (e.g. `js`, `py`, `rust`). Use `path` to scope to a subtree or single file.",
			"- Output modes: `files_with_matches` (default) shows paths newest first, `content` shows matching lines and context in path order, `count` shows regex occurrence counts per file in path order.",
			"- Directory searches include hidden files, respect nested .gitignore rules, and skip symlinks, special files, and version-control directories. An explicit file path bypasses ignore rules.",
			"- `skip` and `head_limit` paginate output entries (including context lines and separators in content mode). Output is limited to 20 KiB; follow the returned skip value for the next page.",
			"- Line scans support lines up to 1 MiB. Multiline and context searches buffer at most 10 MiB per file. Read failures and scan limits return errors, not complete search results.",
			"- Multiline matching: by default patterns match within single lines only. For cross-line patterns like `struct \\{[\\s\\S]*?field`, set `multiline=true`.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Regular expression to search file contents for."},
				"path":    map[string]any{"type": "string", "description": "File or directory to search. Defaults to workspace."},
				"glob":    map[string]any{"type": "string", "description": "Glob filters separated by commas or whitespace (e.g. `*.{ts,tsx},*.js`); prefix ! to exclude. Positive filters are ORed; exclusions take precedence."},
				"type":    map[string]any{"type": "string", "description": "File type filter (js, py, rust, go, java, …)."},
				"output_mode": map[string]any{
					"type":        "string",
					"description": "`content` shows matching lines; `files_with_matches` (default) shows paths; `count` counts non-overlapping regex occurrences, with totals for the returned page.",
					"enum":        []string{"content", "files_with_matches", "count"},
					"default":     "files_with_matches",
				},
				"-B": map[string]any{"type": "integer", "minimum": 0, "description": "Lines shown before each match (content mode)."},
				"-A": map[string]any{"type": "integer", "minimum": 0, "description": "Lines shown after each match (content mode)."},
				"-C": map[string]any{"type": "integer", "minimum": 0, "description": "Lines shown before and after each match (content mode)."},
				"-n": map[string]any{"type": "boolean", "description": "Show line numbers (content mode). Defaults to true.", "default": true},
				"-i": map[string]any{"type": "boolean", "description": "Case-insensitive search.", "default": false},
				"head_limit": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": fmt.Sprintf("First N entries, any mode. Defaults to %d; 0 removes the entry limit. The output byte limit still applies.", DefaultGrepLimit),
					"default":     DefaultGrepLimit,
				},
				"skip":      map[string]any{"type": "integer", "minimum": 0, "description": "Skip the first N result entries (pagination, not a line number).", "default": 0},
				"multiline": map[string]any{"type": "boolean", "description": "Let patterns span lines (`.` matches newlines).", "default": false},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}
			cfg, err := parseGrepArgs(args)
			if err != nil {
				return tool.Result{}, err
			}
			return executeGrep(ctx, root, cfg, allowedReadRoots)
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
	start, braces := 0, 0
	bracket, escaped := false, false
	for i, r := range glob {
		if escaped {
			escaped = false
			continue
		}
		switch r {
		case '\\':
			escaped = true
		case '[':
			bracket = true
		case ']':
			bracket = false
		case '{':
			if !bracket {
				braces++
			}
		case '}':
			if !bracket && braces > 0 {
				braces--
			}
		case ',', ' ', '\t', '\n', '\r':
			if braces == 0 && !bracket {
				if part := strings.TrimSpace(glob[start:i]); part != "" {
					patterns = append(patterns, part)
				}
				start = i + 1
			}
		}
	}
	if part := strings.TrimSpace(glob[start:]); part != "" {
		patterns = append(patterns, part)
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
	pattern = filepath.ToSlash(pattern)
	basenameOnly := !strings.Contains(pattern, "/")
	for strings.HasPrefix(pattern, "./") {
		pattern = strings.TrimPrefix(pattern, "./")
	}
	if basenameOnly {
		if matched, _ := doublestar.Match(pattern, filepath.Base(path)); matched {
			return true
		}
	}
	if matched, _ := doublestar.Match(pattern, filepath.ToSlash(relPath)); matched {
		return true
	}
	matched, _ := doublestar.Match(pattern, filepath.ToSlash(path))
	return matched
}

var grepTypeExtensions = map[string][]string{
	"c":          {".c"},
	"cpp":        {".cpp", ".cc", ".cxx", ".c++", ".hpp", ".hh", ".hxx", ".h++"},
	"cs":         {".cs"},
	"csharp":     {".cs"},
	"css":        {".css"},
	"dart":       {".dart"},
	"go":         {".go"},
	"h":          {".h", ".hpp", ".hh", ".hxx"},
	"html":       {".html", ".htm"},
	"java":       {".java"},
	"javascript": {".js", ".jsx", ".mjs", ".cjs"},
	"js":         {".js", ".jsx", ".mjs", ".cjs"},
	"json":       {".json"},
	"kotlin":     {".kt", ".kts"},
	"kt":         {".kt", ".kts"},
	"lua":        {".lua"},
	"markdown":   {".md", ".markdown"},
	"md":         {".md", ".markdown"},
	"php":        {".php"},
	"py":         {".py", ".pyw"},
	"python":     {".py", ".pyw"},
	"rb":         {".rb"},
	"ruby":       {".rb"},
	"rs":         {".rs"},
	"rust":       {".rs"},
	"scala":      {".scala", ".sc"},
	"sh":         {".sh", ".bash", ".zsh"},
	"sql":        {".sql"},
	"swift":      {".swift"},
	"toml":       {".toml"},
	"ts":         {".ts", ".mts", ".cts"},
	"tsx":        {".tsx"},
	"typescript": {".ts", ".mts", ".cts", ".tsx"},
	"vue":        {".vue"},
	"yaml":       {".yaml", ".yml"},
	"yml":        {".yaml", ".yml"},
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
	if limitReached && limit > 0 {
		parts = append(parts, fmt.Sprintf("limit: %d", limit))
	}
	if offset > 0 {
		parts = append(parts, fmt.Sprintf("skip: %d", offset))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, ", ")
}

func formatGrepPaginationNotice(limitReached bool, limit, offset int) string {
	info := strings.TrimSpace(formatGrepLimitInfo(limitReached, limit, offset))
	if info == "" {
		if limitReached {
			return "Showing results with pagination"
		}
		return ""
	}
	return "Showing results with pagination = " + info
}

func lineContaining(lineStarts []int, offset int) int {
	i, found := slices.BinarySearch(lineStarts, offset)
	if !found {
		i--
	}
	return max(0, i)
}

func walkGrepFiles(ctx context.Context, fsys fs.FS, root string, onFile func(path, relPath string, d fs.DirEntry) error) error {
	for _, component := range strings.Split(root, "/") {
		if vcsDirs[component] {
			return nil
		}
	}

	cache := newGitignoreCache(ctx, fsys)
	if root != "." && root != "/" {
		ignored, err := cache.ancestorIgnored(root)
		if err != nil {
			return err
		}
		if ignored {
			return nil
		}
	}

	return fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if d.IsDir() {
			if vcsDirs[d.Name()] {
				return filepath.SkipDir
			}
			// The starting directory must be traversed so its own rules can
			// exclude children and selectively reinclude them.
			if path == root {
				return nil
			}
			ignored, err := cache.matches(path, true)
			if err != nil {
				return err
			}
			if ignored {
				return filepath.SkipDir
			}
			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}
		ignored, err := cache.matches(path, false)
		if err != nil {
			return err
		}
		if ignored {
			return nil
		}

		return onFile(path, relPathFromBase(root, path), d)
	})
}

type gitignoreCache struct {
	ctx      context.Context
	fsys     fs.FS
	patterns map[string][]gitignore.Pattern
	matchers map[string]gitignore.Matcher
}

func newGitignoreCache(ctx context.Context, fsys fs.FS) *gitignoreCache {
	return &gitignoreCache{
		ctx:      ctx,
		fsys:     fsys,
		patterns: make(map[string][]gitignore.Pattern),
		matchers: make(map[string]gitignore.Matcher),
	}
}

func (c *gitignoreCache) matches(path string, isDir bool) (bool, error) {
	// A directory's own ignore file governs its children, not the directory.
	matcher, err := c.matcherFor(pathpkg.Dir(path))
	if err != nil {
		return false, err
	}
	if matcher == nil {
		return false, nil
	}

	return matcher.Match(strings.Split(path, "/"), isDir), nil
}

func (c *gitignoreCache) ancestorIgnored(dir string) (bool, error) {
	for current := dir; current != "." && current != "/"; current = pathpkg.Dir(current) {
		ignored, err := c.matches(current, true)
		if err != nil || ignored {
			return ignored, err
		}
	}
	return false, nil
}

func (c *gitignoreCache) matcherFor(dir string) (gitignore.Matcher, error) {
	if cached, ok := c.matchers[dir]; ok {
		return cached, nil
	}
	patterns, err := c.patternsFor(dir)
	if err != nil {
		return nil, err
	}
	if len(patterns) == 0 {
		c.matchers[dir] = nil
		return nil, nil
	}
	m := gitignore.NewMatcher(patterns)
	c.matchers[dir] = m
	return m, nil
}

func (c *gitignoreCache) patternsFor(dir string) ([]gitignore.Pattern, error) {
	if cached, ok := c.patterns[dir]; ok {
		return cached, nil
	}

	var parentPatterns []gitignore.Pattern
	if dir == "." || dir == "/" {
		parentPatterns = nil
	} else {
		var err error
		parentPatterns, err = c.patternsFor(pathpkg.Dir(dir))
		if err != nil {
			return nil, err
		}
		if len(parentPatterns) > 0 && gitignore.NewMatcher(parentPatterns).Match(strings.Split(dir, "/"), true) {
			c.patterns[dir] = parentPatterns
			return parentPatterns, nil
		}
	}

	local, err := loadGitignore(c.ctx, c.fsys, pathDomain(dir))
	if err != nil {
		return nil, fmt.Errorf("read ignore rules in %s: %w", dir, err)
	}
	if len(local) == 0 {
		c.patterns[dir] = parentPatterns
		return parentPatterns, nil
	}

	combined := make([]gitignore.Pattern, 0, len(parentPatterns)+len(local))
	combined = append(combined, parentPatterns...)
	combined = append(combined, local...)
	c.patterns[dir] = combined
	return combined, nil
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
