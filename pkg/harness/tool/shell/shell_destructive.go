package shell

import (
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const (
	// maxDestructiveParseBytes bounds the text handed to the parser, whose cost
	// grows with the square of a command's argument count. Longer text is
	// scanned word by word instead.
	maxDestructiveParseBytes = 32 << 10
	// maxNestedScripts bounds how deep sh -c, eval and similar are followed.
	maxNestedScripts = 4
	// scanArgumentWindow bounds the arguments a word scan reads per command,
	// which keeps the scan linear in the text.
	scanArgumentWindow = 64
	// maxBraceWords bounds the words one brace expression is expanded into.
	maxBraceWords = 64
)

// RequiresApproval reports whether a command run with default permissions is
// reviewed first. Inside the OS sandbox only destructive commands are; without
// it the full classifier decides. dirs are passed to IsDestructiveCommand.
func RequiresApproval(command string, opts *Options, dirs ...string) bool {
	return requiresApproval(command, workspaceSandboxEnabled(opts), dirs...)
}

// protectedNamesEnforced reports whether the OS sandbox refuses protected
// configuration names that do not exist yet. Seatbelt does. The mount-based
// backends cover only the paths present at launch.
var protectedNamesEnforced = runtime.GOOS == "darwin"

func requiresApproval(command string, sandboxed bool, dirs ...string) bool {
	if !sandboxed {
		return IsDangerousCommand(command)
	}
	// Where the sandbox cannot stop a command from creating protected
	// configuration, commands that name it keep the full classifier.
	if !protectedNamesEnforced && namesProtectedConfiguration(command) && IsDangerousCommand(command) {
		return true
	}
	return IsDestructiveCommand(command, dirs...)
}

func namesProtectedConfiguration(command string) bool {
	for _, name := range slices.Concat(protectedDirs, protectedFiles, protectedRootFiles) {
		if strings.Contains(command, name) {
			return true
		}
	}
	return false
}

// IsDestructiveCommand reports the commands that are reviewed even inside the
// OS sandbox, which limits what a command reaches but not what it destroys in
// the workspace or publishes from it:
//
//   - git discarding work across the repository: reset --hard, a forced clean,
//     a whole-tree restore or checkout, a forced push
//   - privilege elevation: sudo, doas, su
//   - a download run by a shell: curl | sh, bash <(curl ...), eval "$(curl ...)"
//   - recursive deletion of the working directory, the home directory, one of
//     dirs, anything containing them, or a .git directory
//
// dirs are the directories the command works in, normally the workspace and
// the working directory; they let an absolute path be recognised. Relative
// paths are read as written, without following cd.
//
// Unlike IsDangerousCommand this is not a security boundary. What it cannot
// read is left to the sandbox: text the parser rejects is only scanned for the
// same four categories, never reported for being unreadable.
func IsDestructiveCommand(command string, dirs ...string) bool {
	return isDestructiveCommandDialect(command, platformShellDialect(), dirs...)
}

func isDestructiveCommandDialect(command string, dialect shellDialect, dirs ...string) bool {
	return isDestructiveScript(command, dialect, broadRoots(dirs), 0)
}

func isDestructiveScript(script string, dialect shellDialect, roots []string, depth int) bool {
	script = strings.TrimSpace(script)
	if script == "" {
		return false
	}
	if len(script) > maxDestructiveParseBytes || depth > maxNestedScripts {
		return scanDestructiveWords(script, dialect, roots)
	}
	syntax := parseShellSyntax(script, dialect)
	// Heredoc bodies are blanked in executableSource, so their text is not scanned.
	if syntax.uncertain && scanDestructiveWords(syntax.executableSource, dialect, roots) {
		return true
	}
	if syntax.downloadToShell {
		return true
	}
	for _, statement := range syntax.statements {
		if isDestructiveStatement(statement, dialect, roots, depth) {
			return true
		}
	}
	return false
}

func isDestructiveStatement(statement string, dialect shellDialect, roots []string, depth int) bool {
	statement = strings.TrimSpace(statement)
	if dialect == dialectPowerShell {
		// The backtick escapes there and the backslash only separates paths.
		statement = strings.ReplaceAll(decodePowerShellBackticks(statement), `\`, "/")
	}
	words, ok := splitShellWords(plainDollarQuotes(statement))
	if !ok {
		return scanDestructiveWords(statement, dialect, roots)
	}
	// A recovered statement can still lead with a group, negation or keyword.
	for len(words) > 0 {
		if lead := strings.TrimLeft(words[0], "({!"); lead != "" && !shellKeywords[lead] {
			words = append([]string{lead}, words[1:]...)
			break
		}
		words = words[1:]
	}
	return isDestructiveWords(words, statement, dialect, roots, depth)
}

// isDestructiveWords judges one command. source is the text the words came
// from, when they are a whole statement.
func isDestructiveWords(words []string, source string, dialect shellDialect, roots []string, depth int) bool {
	wrapped := words
	words, cmd, unresolved := unwrapCommandWords(words)
	if unresolved {
		return false
	}
	// "command -v sudo" looks the name up and runs nothing.
	for i, wrapper := range wrapped[:len(wrapped)-len(words)] {
		if i > 0 && (wrapper == "-v" || wrapper == "-V") && commandBase(wrapped[i-1]) == "command" {
			return false
		}
	}
	args := words[1:]
	if elevatesPrivilege(cmd) || destroysFiles(cmd, args, dialect, roots) {
		return true
	}
	switch cmd {
	case "sh", "bash", "zsh", "fish", "dash", "ksh":
		return isDestructiveScript(extractShellScript(args), dialectBash, roots, depth+1)
	case "powershell", "pwsh":
		return isDestructiveScript(decodePowerShellBackticks(extractPowerShellScript(args)), dialectPowerShell, roots, depth+1)
	case "cmd":
		// The PowerShell rules also read cmd.exe spellings such as rd /s.
		return isDestructiveScript(decodeCmdCarets(extractCmdScript(args)), dialectPowerShell, roots, depth+1)
	case "trap":
		return isDestructiveScript(trapActionArgs(args), dialectBash, roots, depth+1)
	case "eval", "invoke-expression", "iex":
		return evaluatesDownload(source, dialect) || isDestructiveScript(strings.Join(args, " "), dialect, roots, depth+1)
	case "source", ".":
		return evaluatesDownload(source, dialect)
	case "find":
		for i, arg := range args {
			if findRunsCommand(arg) && isDestructiveWords(findPayload(args[i+1:]), "", dialect, roots, depth) {
				return true
			}
		}
	}
	return false
}

func elevatesPrivilege(cmd string) bool {
	return cmd == "sudo" || cmd == "doas" || cmd == "su"
}

// destroysFiles covers the categories that are decided by one command's own
// arguments, without reading a script nested in them.
func destroysFiles(cmd string, args []string, dialect shellDialect, roots []string) bool {
	switch cmd {
	case "git":
		return gitDiscardsWork(args, roots)
	case "find":
		return findRemovesBroadTree(args, roots)
	}
	return removesBroadTarget(cmd, args, dialect, roots)
}

// evaluatesDownload reports eval, source or Invoke-Expression given the output
// of a download nested in its own arguments: the same remote code as curl | sh.
func evaluatesDownload(statement string, dialect shellDialect) bool {
	if !strings.ContainsAny(statement, "(`") {
		return false
	}
	for _, nested := range parseShellSyntax(statement, dialect).statements {
		if nested == statement {
			continue
		}
		if isDownloadCommand(nested) || dialect == dialectPowerShell && isPowerShellDownloadCommand(nested) {
			return true
		}
	}
	return false
}

// gitDiscardsWork reports git commands that drop uncommitted work across the
// working tree or overwrite remote history. Stashes and deleted branches stay
// in the object database and are not reported.
func gitDiscardsWork(args []string, roots []string) bool {
	rest := gitOperands(args)
	options, operands := splitOptions(rest)
	long := func(names ...string) bool {
		return slices.ContainsFunc(options, func(option string) bool { return slices.Contains(names, option) })
	}
	wholeTree := func() bool {
		return slices.ContainsFunc(operands, func(pathspec string) bool { return isWholeTreePathspec(pathspec, roots) })
	}

	switch firstNonFlagArg(args) {
	case "reset":
		return long("--hard")
	case "clean":
		return (long("--force") || hasShortFlag(options, 'f', "e")) && !long("--dry-run") && !hasShortFlag(options, 'n', "e")
	case "restore":
		staged := long("--staged") || hasShortFlag(options, 'S', "s")
		worktree := long("--worktree") || hasShortFlag(options, 'W', "s")
		return (worktree || !staged) && wholeTree()
	case "checkout":
		if wholeTree() {
			return true
		}
		// Forcing a switch drops every local change; with paths named after
		// "--" it only overwrites those files.
		terminator := slices.Index(rest, "--")
		namesPaths := terminator >= 0 && terminator+1 < len(rest)
		return (long("--force") || hasShortFlag(options, 'f', "bBU")) && !namesPaths
	case "switch":
		return long("--force", "--discard-changes") || hasShortFlag(options, 'f', "cC")
	case "push":
		if long("--dry-run") || hasShortFlag(options, 'n', "o") {
			return false
		}
		return long("--force", "--force-with-lease", "--mirror") || hasShortFlag(options, 'f', "o") ||
			slices.ContainsFunc(options, func(option string) bool { return strings.HasPrefix(option, "--force-with-lease=") }) ||
			slices.ContainsFunc(operands, func(refspec string) bool { return strings.HasPrefix(refspec, "+") })
	}
	return false
}

func isWholeTreePathspec(pathspec string, roots []string) bool {
	// ":/" and ":(top)" anchor a pathspec at the repository root.
	for _, magic := range []string{":(top)", ":/"} {
		if rest, ok := strings.CutPrefix(pathspec, magic); ok {
			pathspec = "./" + rest
			break
		}
	}
	return isBroadPath(pathspec, roots)
}

// removesBroadTarget reports rm -r, and Remove-Item -Recurse with its aliases
// in PowerShell, on a broad path or a .git directory.
func removesBroadTarget(cmd string, args []string, dialect shellDialect, roots []string) bool {
	recursive := false
	var operands []string
	switch {
	case dialect == dialectPowerShell && slices.Contains([]string{"rm", "remove-item", "ri", "del", "erase", "rd", "rmdir"}, cmd):
		// del and rd also take cmd.exe switches such as /s /q.
		switches := cmd != "rm" && cmd != "remove-item" && cmd != "ri"
		for _, arg := range args {
			switch {
			case powerShellParameterAbbreviates(arg, "-recurse", 1), rmOptionIsRecursive(arg), switches && strings.EqualFold(arg, "/s"):
				recursive = true
			case strings.HasPrefix(arg, "-"), switches && len(arg) == 2 && arg[0] == '/':
			default:
				operands = append(operands, strings.Split(arg, ",")...)
			}
		}
	case cmd == "rm":
		var options []string
		options, operands = splitOptions(args)
		recursive = slices.ContainsFunc(options, rmOptionIsRecursive)
	}
	return recursive && slices.ContainsFunc(operands, func(operand string) bool { return removalIsBroad(operand, roots) })
}

func rmOptionIsRecursive(option string) bool {
	if strings.HasPrefix(option, "--") {
		return len(option) >= len("--r") && strings.HasPrefix("--recursive", option)
	}
	flags := strings.TrimPrefix(option, "-")
	return flags != option && strings.ContainsAny(flags, "rR") && strings.Trim(flags, "rRfidvIPWx") == ""
}

// Primaries that act or steer the walk without narrowing which files match.
var findNeutralPrimaries = map[string]bool{
	"-delete": true, "-print": true, "-print0": true, "-ls": true, "-prune": true,
	"-depth": true, "-d": true, "-xdev": true, "-mount": true, "-x": true, "-follow": true,
	"-noleaf": true, "-true": true, "-daystart": true, "-warn": true, "-nowarn": true,
	"-a": true, "-and": true, "-o": true, "-or": true, "-not": true,
}

// findRemovesBroadTree reports find deleting everything below a broad starting
// point: -delete or an rm payload with no test that narrows the matches.
// Deleting by name, age or size is routine cleanup and is not reported.
func findRemovesBroadTree(args []string, roots []string) bool {
	for len(args) > 0 {
		if arg := args[0]; arg == "-D" && len(args) > 1 {
			args = args[2:]
		} else if slices.Contains([]string{"-H", "-L", "-P", "-E", "-X", "-d", "-s", "-x"}, arg) || strings.HasPrefix(arg, "-O") {
			args = args[1:]
		} else {
			break
		}
	}
	var starts []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") && !slices.Contains([]string{"(", ")", "!", ","}, args[0]) {
		starts = append(starts, args[0])
		args = args[1:]
	}
	if len(starts) == 0 {
		starts = []string{"."}
	}

	deletes, narrowed := false, false
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "-delete":
			deletes = true
		case findRunsCommand(arg):
			payload := findPayload(args[i+1:])
			if _, cmd, unresolved := unwrapCommandWords(payload); !unresolved && cmd == "rm" {
				deletes = true
			}
			i += len(payload)
		case slices.Contains([]string{"-mindepth", "-maxdepth", "-type", "-xtype", "-regextype"}, arg):
			i++
		case slices.Contains([]string{"-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename"}, arg):
			i++
			// "! -name ." only spares the starting point itself.
			narrowed = narrowed || i >= len(args) || args[i] != "." && !catchAllPatterns[strings.TrimPrefix(args[i], "./")]
		default:
			narrowed = narrowed || strings.HasPrefix(arg, "-") && !findNeutralPrimaries[arg]
		}
	}
	return deletes && !narrowed && slices.ContainsFunc(starts, func(start string) bool { return removalIsBroad(start, roots) })
}

func findRunsCommand(primary string) bool {
	return primary == "-exec" || primary == "-execdir" || primary == "-ok" || primary == "-okdir"
}

func findPayload(args []string) []string {
	for i, arg := range args {
		if arg == ";" || arg == "+" {
			return args[:i]
		}
	}
	return args
}

// splitOptions separates dash-prefixed words from operands; everything after
// "--" is an operand. The value of an option that takes one stays an operand.
func splitOptions(args []string) (options, operands []string) {
	for i, arg := range args {
		if arg == "--" {
			return options, append(operands, args[i+1:]...)
		}
		if len(arg) > 1 && arg[0] == '-' {
			options = append(options, arg)
		} else {
			operands = append(operands, arg)
		}
	}
	return options, operands
}

// hasShortFlag reports flag in a single-dash cluster such as -fdx. Reading a
// cluster stops at a letter listed in valued, which takes the rest as its value.
func hasShortFlag(options []string, flag byte, valued string) bool {
	for _, option := range options {
		if strings.HasPrefix(option, "--") {
			continue
		}
		for i := 1; i < len(option); i++ {
			if option[i] == flag {
				return true
			}
			if strings.IndexByte(valued, option[i]) >= 0 {
				break
			}
		}
	}
	return false
}

func removalIsBroad(operand string, roots []string) bool {
	return slices.ContainsFunc(expandBraces(operand), func(target string) bool {
		return path.Base(trimWildcardTail(target)) == ".git" || isBroadPath(target, roots)
	})
}

// Spellings of the home and current directory that are read without a shell.
var (
	homeAnchors    = []string{"~", "$home", "${home}", "$env:userprofile", "${env:userprofile}", "$env:home", "%userprofile%"}
	workdirAnchors = []string{"$pwd", "${pwd}", "$(pwd)", "`pwd`", "%cd%", "$(git rev-parse --show-toplevel)"}
)

// Glob components that match every name, or every hidden name, in a directory.
var catchAllPatterns = map[string]bool{
	"*": true, "**": true, "*.*": true, "?*": true,
	".*": true, ".[!.]*": true, ".[^.]*": true, "..?*": true, ".??*": true,
}

// isBroadPath reports a path that names the directory a command runs in, the
// home directory, one of roots, or a directory containing any of them, with or
// without a trailing wildcard. A named file or subdirectory is not broad, nor
// is a path built on any other variable: its value is not known here.
func isBroadPath(target string, roots []string) bool {
	target = strings.ReplaceAll(target, `\`, "/")
	if rest, ok := cutPathAnchor(target, workdirAnchors); ok {
		target = "." + rest
	} else if rest, ok := cutPathAnchor(target, homeAnchors); ok {
		if isBroadPath("."+rest, nil) {
			return true
		}
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return false
		}
		target = filepath.ToSlash(home) + rest
	}
	if strings.ContainsAny(target, "$`") {
		return false
	}
	target = trimWildcardTail(target)
	if target == "" {
		return false
	}
	// A drive root is spelled out so the PowerShell dialect reads alike on every host.
	if len(target) == 2 && isASCIILetter(target[0]) && target[1] == ':' {
		return true
	}
	if strings.HasPrefix(target, "/") || filepath.IsAbs(filepath.FromSlash(target)) {
		clean := filepath.Clean(filepath.FromSlash(target))
		return filepath.Dir(clean) == clean || slices.ContainsFunc(roots, func(root string) bool { return containsPath(clean, root) })
	}
	for part := range strings.SplitSeq(path.Clean(target), "/") {
		if part != "." && part != ".." && !catchAllPatterns[part] {
			return false
		}
	}
	return true
}

func cutPathAnchor(target string, anchors []string) (rest string, ok bool) {
	for _, anchor := range anchors {
		if len(target) >= len(anchor) && strings.EqualFold(target[:len(anchor)], anchor) &&
			(len(target) == len(anchor) || target[len(anchor)] == '/') {
			return target[len(anchor):], true
		}
	}
	return "", false
}

// trimWildcardTail normalises separators and drops trailing slashes and
// catch-all components, so that dir/* is judged as dir and * as ".".
func trimWildcardTail(target string) string {
	target = strings.ReplaceAll(target, `\`, "/")
	for {
		trimmed := strings.TrimRight(target, "/")
		if trimmed == "" {
			return target[:min(len(target), 1)]
		}
		dir, last := path.Split(trimmed)
		if !catchAllPatterns[last] {
			return trimmed
		}
		if dir == "" {
			return "."
		}
		target = dir
	}
}

// broadRoots lists the directories whose removal by absolute path is broad:
// dirs and the home directory, each as given and with symlinks resolved.
func broadRoots(dirs []string) []string {
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(slices.Clone(dirs), home)
	}
	var roots []string
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		for _, spelling := range []string{filepath.Clean(dir), canonicalPath(dir)} {
			if !slices.Contains(roots, spelling) {
				roots = append(roots, spelling)
			}
		}
	}
	return roots
}

func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// expandBraces lists the words that brace alternations such as {a,b}/x stand
// for. Sequences ({1..3}) are kept as written, as is a word with more than
// maxBraceWords expansions.
func expandBraces(word string) []string {
	pending := []string{word}
	var expanded []string
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		alternatives := expandFirstBrace(next)
		if alternatives == nil {
			expanded = append(expanded, next)
			continue
		}
		pending = append(pending, alternatives...)
		if len(pending)+len(expanded) > maxBraceWords {
			return []string{word}
		}
	}
	return expanded
}

// expandFirstBrace expands the first {a,b} group of word, or returns nil.
func expandFirstBrace(word string) []string {
	var parts []string
	depth, open, start := 0, 0, 0
	for i := 0; i < len(word); i++ {
		switch word[i] {
		case '{':
			if depth++; depth == 1 {
				open, start, parts = i, i+1, nil
			}
		case ',':
			if depth == 1 {
				parts = append(parts, word[start:i])
				start = i + 1
			}
		case '}':
			if depth == 0 {
				continue
			}
			if depth--; depth > 0 || len(parts) == 0 {
				continue
			}
			alternatives := make([]string, 0, len(parts)+1)
			for _, part := range append(parts, word[start:i]) {
				alternatives = append(alternatives, word[:open]+part+word[i+1:])
			}
			return alternatives
		}
	}
	return nil
}

// plainDollarQuotes rewrites $'...' and $"..." as ordinary quoted words, which
// splitShellWords accepts. Escape sequences in $'...' become placeholders: the
// word is an argument whose exact bytes do not decide any category.
func plainDollarQuotes(text string) string {
	if !strings.Contains(text, "$'") && !strings.Contains(text, `$"`) {
		return text
	}
	var out strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		switch {
		case inSingle:
			inSingle = ch != '\''
		case ch == '\\' && i+1 < len(text):
			out.WriteByte(ch)
			i++
			ch = text[i]
		case ch == '"':
			inDouble = !inDouble
		case inDouble:
		case ch == '\'':
			inSingle = true
		case ch == '$' && i+1 < len(text) && text[i+1] == '"':
			continue
		case ch == '$' && i+1 < len(text) && text[i+1] == '\'':
			out.WriteByte('\'')
			for i += 2; i < len(text) && text[i] != '\''; i++ {
				if text[i] == '\\' && i+1 < len(text) {
					i++
					out.WriteByte('_')
					continue
				}
				out.WriteByte(text[i])
			}
			ch = '\''
		}
		out.WriteByte(ch)
	}
	return out.String()
}

// Commands that run their input or arguments as a script.
var scriptInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true,
	"eval": true, "source": true, ".": true,
	"powershell": true, "pwsh": true, "invoke-expression": true, "iex": true,
}

type wordSegment struct {
	words []string
	// pipeline is false when the segment ends its command list.
	pipeline bool
}

// scanDestructiveWords is the fallback for text the parser cannot structure.
// It ignores quoting and nesting, so that a quoted script is read too: every
// word may name a command whose arguments run to the next control operator.
// Privilege elevation is only read where a command starts, since prose in
// such text would otherwise match.
func scanDestructiveWords(text string, dialect shellDialect, roots []string) bool {
	// A download and an interpreter in different stages of one pipeline or
	// substitution are remote code, whichever is written first.
	var interpreters, downloads, both int
	for _, segment := range splitWordSegments(text, dialect) {
		download := false
		for i, word := range segment.words {
			cmd := commandBase(word)
			args := segment.words[i+1:]
			if destroysFiles(cmd, args[:min(len(args), scanArgumentWindow)], dialect, roots) {
				return true
			}
			download = download || isDownloadCommand(word) || dialect == dialectPowerShell && isPowerShellDownloadCommand(word)
		}
		_, cmd, unresolved := unwrapCommandWords(segment.words)
		if !unresolved && elevatesPrivilege(cmd) {
			return true
		}
		interpreter := !unresolved && scriptInterpreters[cmd]
		if interpreter {
			interpreters++
		}
		if download {
			downloads++
		}
		if interpreter && download {
			both++
		}
		if interpreters > 0 && downloads > 0 && !(interpreters == 1 && downloads == 1 && both == 1) {
			return true
		}
		if !segment.pipeline {
			interpreters, downloads, both = 0, 0, 0
		}
	}
	return false
}

// splitWordSegments splits text at shell control operators, drops quotes and
// escapes, and splits each piece at blanks.
func splitWordSegments(text string, dialect shellDialect) []wordSegment {
	var segments []wordSegment
	var words []string
	var word strings.Builder
	endWord := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	endSegment := func(pipeline bool) {
		endWord()
		segments = append(segments, wordSegment{words: words, pipeline: pipeline})
		words = nil
	}
	for i := 0; i < len(text); i++ {
		next := byte(0)
		if i+1 < len(text) {
			next = text[i+1]
		}
		ch := text[i]
		if dialect == dialectPowerShell && (ch == '\\' || ch == '`') {
			// The backslash separates paths there and the backtick escapes.
			if ch == '\\' {
				word.WriteByte('/')
			}
			continue
		}
		switch ch {
		case '\'', '"':
		case '\\':
			if i++; next != 0 && next != '\n' {
				word.WriteByte(next)
			}
		case ' ', '\t', '\r':
			endWord()
		case ';', '\n':
			endSegment(false)
		case '&':
			// "&>" and ">&" redirect; "&" and "&&" end the pipeline.
			if next != '>' && (i == 0 || text[i-1] != '>' && text[i-1] != '<') {
				endSegment(false)
			}
		case '|':
			// "||" ends the pipeline; "|&" continues it.
			if next == '|' || next == '&' {
				i++
			}
			endSegment(next != '|')
		case '(', ')', '`', '<', '>':
			endSegment(true)
		default:
			word.WriteByte(ch)
		}
	}
	endSegment(false)
	return segments
}
