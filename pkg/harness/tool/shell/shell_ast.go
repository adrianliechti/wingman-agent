package shell

import (
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

type shellDialect uint8

const (
	dialectBash shellDialect = iota
	dialectPowerShell
)

type shellStatement struct {
	text       string
	start, end uint32
}

type shellSyntax struct {
	executableSource string
	statements       []string
	hasSubstitution  bool
	hasRedirection   bool
	definesFunction  bool
	protectedWrite   bool
	credentialRead   bool
	downloadToShell  bool
	uncertain        bool
}

var (
	bashLanguage = sync.OnceValue(grammars.BashLanguage)
	bashParsers  = sync.Pool{New: func() any { return gotreesitter.NewParser(bashLanguage()) }}

	powerShellLanguage = sync.OnceValue(grammars.PowershellLanguage)
	powerShellParsers  = sync.Pool{New: func() any { return gotreesitter.NewParser(powerShellLanguage()) }}
)

func platformShellDialect() shellDialect {
	if runtime.GOOS == "windows" {
		return dialectPowerShell
	}
	return dialectBash
}

func languageForDialect(dialect shellDialect) *gotreesitter.Language {
	switch dialect {
	case dialectPowerShell:
		return powerShellLanguage()
	default:
		return bashLanguage()
	}
}

func parserPoolForDialect(dialect shellDialect) *sync.Pool {
	if dialect == dialectPowerShell {
		return &powerShellParsers
	}
	return &bashParsers
}

func parseShellSyntax(source string, dialect shellDialect) (result shellSyntax) {
	result.executableSource = source
	if strings.TrimSpace(source) == "" {
		return result
	}
	defer func() {
		if recover() != nil {
			result = shellSyntax{executableSource: source, statements: []string{source}, uncertain: true}
		}
	}()

	lang := languageForDialect(dialect)
	pool := parserPoolForDialect(dialect)
	parser := pool.Get().(*gotreesitter.Parser)
	parserReusable := false
	defer func() {
		if parserReusable {
			pool.Put(parser)
		}
	}()

	sourceBytes := []byte(source)
	parsed := sourceBytes
	if dialect == dialectPowerShell {
		parsed = maskPowerShellArgumentWords(sourceBytes)
	}
	// Node offsets are shared; all text below comes from the original source.
	tree, err := parser.Parse(parsed)
	parserReusable = true
	if err != nil || tree == nil || tree.RootNode() == nil {
		result.statements = []string{source}
		result.uncertain = true
		return result
	}
	// Restoring a token may change the context of later tokens. Validate
	// again after each reparse; every pass only restores original bytes.
	for dialect == dialectPowerShell && restorePowerShellNonArguments(tree.RootNode(), lang, sourceBytes, parsed) {
		tree.Release()
		tree, err = parser.Parse(parsed)
		if err != nil || tree == nil || tree.RootNode() == nil {
			result.statements = []string{source}
			result.uncertain = true
			return result
		}
	}
	defer tree.Release()
	result.uncertain = tree.RootNode().HasErrorOrMissing() && !shellParseErrorsHandled(tree.RootNode(), lang, sourceBytes)

	executable := slices.Clone(sourceBytes)
	var statements []shellStatement
	sanitizeHeredocBodies(tree.RootNode(), lang, sourceBytes, executable, false, &result, &statements)
	result.executableSource = string(executable)

	walkShellAST(tree.RootNode(), lang, executable, dialect, &result, &statements)
	for _, tail := range recoverMalformedHeredocTails(tree.RootNode(), lang, sourceBytes) {
		tailSyntax := parseShellSyntax(tail.text, dialect)
		result.hasSubstitution = result.hasSubstitution || tailSyntax.hasSubstitution
		result.hasRedirection = result.hasRedirection || tailSyntax.hasRedirection
		result.definesFunction = result.definesFunction || tailSyntax.definesFunction
		result.protectedWrite = result.protectedWrite || tailSyntax.protectedWrite
		result.credentialRead = result.credentialRead || tailSyntax.credentialRead
		result.downloadToShell = result.downloadToShell || tailSyntax.downloadToShell
		result.uncertain = result.uncertain || tailSyntax.uncertain
		for _, statement := range tailSyntax.statements {
			statements = append(statements, shellStatement{text: statement, start: tail.start, end: tail.end})
		}
	}
	slices.SortStableFunc(statements, func(a, b shellStatement) int {
		if a.start != b.start {
			return int(a.start) - int(b.start)
		}
		return int(b.end) - int(a.end)
	})
	for _, statement := range statements {
		text := strings.TrimSpace(statement.text)
		if text == "" || slices.Contains(result.statements, text) {
			continue
		}
		result.statements = append(result.statements, text)
	}
	if len(result.statements) == 0 && strings.TrimSpace(source) != "" && tree.RootNode().HasErrorOrMissing() {
		result.statements = []string{source}
		result.uncertain = true
	}
	return result
}

func walkShellAST(node *gotreesitter.Node, lang *gotreesitter.Language, source []byte, dialect shellDialect, result *shellSyntax, statements *[]shellStatement) {
	typeName := node.Type(lang)
	switch typeName {
	case "command", "declaration_command", "unset_command", "redirected_statement":
		*statements = append(*statements, shellStatement{text: node.Text(source), start: node.StartByte(), end: node.EndByte()})
		if typeName == "command" && commandContainsDownloadSubstitution(node, lang, source) {
			result.downloadToShell = true
		}
	case "variable_assignment":
		if parent := node.Parent(); parent == nil || parent.Type(lang) != "command" {
			*statements = append(*statements, shellStatement{text: node.Text(source), start: node.StartByte(), end: node.EndByte()})
		}
	case "test_command":
		*statements = append(*statements, shellStatement{text: node.Text(source), start: node.StartByte(), end: node.EndByte()})
	case "function_definition", "function_statement":
		result.definesFunction = true
	case "command_substitution", "process_substitution", "sub_expression":
		result.hasSubstitution = true
	case "heredoc_start":
		// The Bash grammar recovers mixed-quoted delimiters under ERROR rather
		// than a heredoc_redirect, but this is still shell redirection.
		result.hasRedirection = true
	case "file_redirect", "herestring_redirect", "heredoc_redirect", "redirection":
		result.hasRedirection = true
		if target := redirectTarget(node, lang, source); target != "" {
			if redirectWrites(node.Text(source), target) {
				result.protectedWrite = result.protectedWrite || isProtectedRedirectTarget(target)
			} else if typeName == "file_redirect" || typeName == "redirection" {
				result.credentialRead = result.credentialRead || isSensitiveCredentialPath(target)
			}
		}
	case "pipeline":
		if directPipelineIsDangerous(node, lang, source, dialect) {
			result.downloadToShell = true
		}
	}

	for _, child := range node.Children() {
		walkShellAST(child, lang, source, dialect, result, statements)
	}
}

func sanitizeHeredocBodies(
	node *gotreesitter.Node,
	lang *gotreesitter.Language,
	source, executable []byte,
	quotedHeredoc bool,
	result *shellSyntax,
	statements *[]shellStatement,
) {
	children := node.Children()
	for _, child := range children {
		switch child.Type(lang) {
		case "heredoc_start":
			quotedHeredoc = strings.ContainsAny(child.Text(source), `'"\`)
			continue
		case "heredoc_body":
			blankRangePreservingLines(executable, child.StartByte(), child.EndByte())
			if quotedHeredoc {
				continue
			}
			for _, expansion := range child.Children() {
				if expansion.Type(lang) == "heredoc_content" {
					continue
				}
				copy(executable[expansion.StartByte():expansion.EndByte()], source[expansion.StartByte():expansion.EndByte()])
			}
			restoreHeredocBackticks(source, executable, child.StartByte(), child.EndByte(), result, statements)
			continue
		}
		sanitizeHeredocBodies(child, lang, source, executable, quotedHeredoc, result, statements)
	}
}

func restoreHeredocBackticks(
	source, executable []byte,
	start, end uint32,
	result *shellSyntax,
	statements *[]shellStatement,
) {
	sourceText := string(source)
	for i := int(start); i < int(end) && i < len(source); i++ {
		if source[i] == '\\' {
			i++
			continue
		}
		if source[i] != '`' {
			continue
		}
		substitution, close, ok := readBacktickSubstitution(sourceText, i+1)
		if !ok || close >= int(end) {
			continue
		}
		copy(executable[i:close+1], source[i:close+1])
		*statements = append(*statements, shellStatement{text: substitution, start: uint32(i + 1), end: uint32(close)})
		result.hasSubstitution = true
		i = close
	}
}

func recoverMalformedHeredocTails(root *gotreesitter.Node, lang *gotreesitter.Language, source []byte) []shellStatement {
	var tails []shellStatement
	var visit func(*gotreesitter.Node)
	visit = func(node *gotreesitter.Node) {
		if node.Type(lang) == "ERROR" {
			children := node.Children()
			for _, child := range children {
				if child.Type(lang) != "heredoc_start" {
					continue
				}
				rawDelimiter := child.Text(source)
				if !strings.ContainsAny(rawDelimiter, `'"\`) {
					continue
				}
				delimiter := unquoteHeredocDelimiter(rawDelimiter)
				if delimiter == "" {
					continue
				}
				if tailStart, found := heredocTailStart(source, child.EndByte(), delimiter); found && tailStart < node.EndByte() {
					tails = append(tails, shellStatement{text: string(source[tailStart:node.EndByte()]), start: tailStart, end: node.EndByte()})
				}
			}
		}
		for _, child := range node.Children() {
			visit(child)
		}
	}
	visit(root)
	return tails
}

func unquoteHeredocDelimiter(value string) string {
	var delimiter strings.Builder
	inSingle := false
	inDouble := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
				continue
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
				continue
			}
		case '\\':
			if !inSingle && i+1 < len(value) {
				i++
			}
		}
		delimiter.WriteByte(value[i])
	}
	return delimiter.String()
}

func heredocTailStart(source []byte, start uint32, delimiter string) (uint32, bool) {
	for lineStart := int(start); lineStart < len(source); {
		if source[lineStart] == '\r' || source[lineStart] == '\n' {
			lineStart++
			continue
		}
		lineEnd := lineStart
		for lineEnd < len(source) && source[lineEnd] != '\r' && source[lineEnd] != '\n' {
			lineEnd++
		}
		if strings.TrimLeft(string(source[lineStart:lineEnd]), "\t") == delimiter {
			for lineEnd < len(source) && (source[lineEnd] == '\r' || source[lineEnd] == '\n') {
				lineEnd++
			}
			return uint32(lineEnd), true
		}
		lineStart = lineEnd
	}
	return uint32(len(source)), false
}

// maskPowerShellArgumentWords works around grammar gaps for native-command
// arguments such as -j8, -count=1, --pretty=oneline, -O2, and a bare --,
// which PowerShell's argument mode passes through as literal text. Inside
// such a dash word, characters the grammar trips over become letters of the
// same length, so the word stays one bareword. Words with any character that
// is meaningful to PowerShell (variables, subexpressions, quotes, operators,
// splatting, escapes, or the --% stop-parsing token) are left untouched and
// still fail closed. These are provisional replacements: after parsing,
// restorePowerShellNonArguments restores anything outside a complete command
// argument before the tree is used for classification.
func maskPowerShellArgumentWords(source []byte) []byte {
	var masked []byte
	for start := 0; start < len(source); {
		if source[start] != '-' || start == 0 || !isPowerShellArgumentSeparator(source[start-1]) || !followsPowerShellWord(source[:start]) {
			start++
			continue
		}
		end := start
		for end < len(source) && !isPowerShellArgumentEnd(source[end]) {
			end++
		}
		word := source[start:end]
		if needsPowerShellArgumentMask(word) {
			if masked == nil {
				masked = slices.Clone(source)
			}
			for i := start; i < end; i++ {
				if c := masked[i]; c != '-' && !isASCIILetter(c) {
					masked[i] = 'x'
				}
			}
			if strings.Trim(string(word), "-") == "" {
				masked[start] = 'x' // a bare -- (end of parameters) becomes a plain word
			}
		}
		start = end
	}
	if masked == nil {
		return source
	}
	return masked
}

// Only a complete parameter or bareword directly in command_elements may
// retain a replacement. In particular, quoted text, comments, command names,
// expressions and dynamic arguments must retain their original parse.
func restorePowerShellNonArguments(root *gotreesitter.Node, lang *gotreesitter.Language, original, parsed []byte) bool {
	if slices.Equal(original, parsed) {
		return false
	}
	allowed := make([]bool, len(parsed))
	var visit func(*gotreesitter.Node)
	visit = func(node *gotreesitter.Node) {
		parent := node.Parent()
		if (node.Type(lang) == "command_parameter" || node.Type(lang) == "generic_token") && parent != nil && parent.Type(lang) == "command_elements" {
			start, end := int(node.StartByte()), int(node.EndByte())
			if start > 0 && isPowerShellArgumentSeparator(original[start-1]) && (end == len(original) || isPowerShellArgumentEnd(original[end])) && needsPowerShellArgumentMask(original[start:end]) {
				for i := start; i < end; i++ {
					allowed[i] = true
				}
			}
		}
		for _, child := range node.Children() {
			visit(child)
		}
	}
	visit(root)
	restored := false
	for i := range parsed {
		if parsed[i] != original[i] && !allowed[i] {
			parsed[i] = original[i]
			restored = true
		}
	}
	return restored
}

func needsPowerShellArgumentMask(word []byte) bool {
	if string(word) == "--%" {
		return false
	}
	rest := strings.TrimLeft(string(word), "-")
	if rest == "" {
		return len(word) == 2
	}
	needs := false
	for _, c := range []byte(rest) {
		switch {
		case isASCIILetter(c), c == '-', c == '_':
		case c >= '0' && c <= '9', strings.IndexByte("=:.%+/~", c) >= 0:
			needs = true
		default:
			return false
		}
	}
	return needs
}

func isPowerShellArgumentSeparator(c byte) bool { return c == ' ' || c == '\t' }

func isPowerShellArgumentEnd(c byte) bool {
	return isPowerShellArgumentSeparator(c) || strings.IndexByte("\r\n;|)}", c) >= 0
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// followsPowerShellWord reports whether the dash word continues a command:
// the previous non-blank character on its line must end another word.
func followsPowerShellWord(before []byte) bool {
	for _, b := range slices.Backward(before) {
		if !isPowerShellArgumentSeparator(b) {
			return strings.IndexByte(";|&({}\n\r=,", b) < 0
		}
	}
	return false
}

func shellParseErrorsHandled(node *gotreesitter.Node, lang *gotreesitter.Language, source []byte) bool {
	if node.IsMissing() {
		return false
	}
	if node.IsError() {
		for _, child := range node.Children() {
			if child.Type(lang) != "heredoc_start" {
				continue
			}
			rawDelimiter := child.Text(source)
			if !strings.ContainsAny(rawDelimiter, `'"\`) {
				continue
			}
			delimiter := unquoteHeredocDelimiter(rawDelimiter)
			if delimiter != "" {
				_, found := heredocTailStart(source, child.EndByte(), delimiter)
				return found
			}
		}
		return false
	}
	for _, child := range node.Children() {
		if !shellParseErrorsHandled(child, lang, source) {
			return false
		}
	}
	return true
}

func blankRangePreservingLines(value []byte, start, end uint32) {
	for i := int(start); i < int(end) && i < len(value); i++ {
		if value[i] != '\n' && value[i] != '\r' {
			value[i] = ' '
		}
	}
}

func redirectTarget(node *gotreesitter.Node, lang *gotreesitter.Language, source []byte) string {
	children := node.Children()
	for _, child := range slices.Backward(children) {
		typeName := child.Type(lang)
		if !child.IsNamed() || typeName == "heredoc_body" || typeName == "heredoc_end" || typeName == "heredoc_start" {
			continue
		}
		return strings.TrimSpace(child.Text(source))
	}
	return ""
}

func redirectWrites(source, target string) bool {
	// Inspect only the operator region, not a quoted filename containing >.
	if end := strings.LastIndex(source, target); end >= 0 {
		return strings.Contains(source[:end], ">")
	}
	return true
}

func directPipelineIsDangerous(node *gotreesitter.Node, lang *gotreesitter.Language, source []byte, dialect shellDialect) bool {
	var commands []string
	collectDirectPipelineCommands(node, lang, source, node, &commands)
	downloadSeen := false
	for _, command := range commands {
		if dialect == dialectPowerShell {
			words, ok := splitShellWords(command)
			if ok && len(words) > 0 {
				cmd := commandBase(strings.Trim(words[0], `(){}[]`))
				if downloadSeen && (cmd == "invoke-expression" || cmd == "iex" || cmd == "pwsh" || cmd == "powershell") {
					return true
				}
			}
			// Git for Windows and MSYS put POSIX shells on PATH.
			if downloadSeen && isShellInterpreter(command) {
				return true
			}
			downloadSeen = downloadSeen || isPowerShellDownloadCommand(command)
			continue
		}
		if downloadSeen && isShellInterpreter(command) {
			return true
		}
		downloadSeen = downloadSeen || isDownloadCommand(command)
	}
	return false
}

func collectDirectPipelineCommands(node *gotreesitter.Node, lang *gotreesitter.Language, source []byte, root *gotreesitter.Node, commands *[]string) {
	if node != root {
		switch node.Type(lang) {
		case "command":
			*commands = append(*commands, node.Text(source))
			return
		case "pipeline":
			return
		}
	}
	for _, child := range node.Children() {
		collectDirectPipelineCommands(child, lang, source, root, commands)
	}
}

func commandContainsDownloadSubstitution(node *gotreesitter.Node, lang *gotreesitter.Language, source []byte) bool {
	words, ok := splitShellWords(strings.TrimSpace(node.Text(source)))
	if !ok || len(words) == 0 {
		return false
	}
	_, cmd, unresolved := unwrapCommandWords(words)
	if unresolved || !slices.Contains([]string{"sh", "bash", "zsh", "fish", "dash", "ksh"}, cmd) {
		return false
	}
	return descendantCommandMatches(node, lang, source, func(command string) bool { return isDownloadCommand(command) })
}

func descendantCommandMatches(node *gotreesitter.Node, lang *gotreesitter.Language, source []byte, match func(string) bool) bool {
	for _, child := range node.Children() {
		if child.Type(lang) == "command" && match(child.Text(source)) {
			return true
		}
		if descendantCommandMatches(child, lang, source, match) {
			return true
		}
	}
	return false
}
