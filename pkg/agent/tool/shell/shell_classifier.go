package shell

import (
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// ClassifyEffect maps a shell-tool invocation to one of three effect tiers:
// - EffectDangerous → prompts the user before running
// - EffectReadOnly  → runs in plan mode (no prompt)
// - EffectMutates   → runs in normal mode without prompt, blocked in plan mode
func ClassifyEffect(args map[string]any) tool.Effect {
	if args == nil {
		return tool.EffectDynamic
	}

	command, _ := args["command"].(string)
	if IsDangerousCommand(command) {
		return tool.EffectDangerous
	}
	if IsReadOnlyCommand(command) {
		return tool.EffectReadOnly
	}

	return tool.EffectMutates
}

// IsDangerousCommand reports whether the command requires user confirmation
// before running. Kept narrow on purpose: only commands that are hard to
// reverse or that escalate privileges. Routine mutations (chmod, kill,
// find -delete, ...) are not dangerous — they fall through to EffectMutates
// and run automatically.
func IsDangerousCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	if hasDangerousCommandSubstitution(command) {
		return true
	}

	segments := splitCommandSegments(command)
	for i, seg := range segments {
		if isDangerousSingleCommand(seg) {
			return true
		}
		if i > 0 && isShellInterpreter(seg) && isDownloadCommand(segments[i-1]) {
			return true
		}
	}

	return false
}

// IsReadOnlyCommand reports whether every segment in the command is on the
// read-only allowlist and the command contains no mutation syntax (>, <, sed -i).
func IsReadOnlyCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	if hasShellCommandSubstitution(command) {
		return false
	}
	if hasMutationSyntax(command) {
		return false
	}

	for _, seg := range splitCommandSegments(command) {
		if !isSingleCommandReadOnly(seg) {
			return false
		}
	}

	return true
}

func hasMutationSyntax(command string) bool {
	if containsUnquotedShellRedirection(command) {
		return true
	}

	words := strings.Fields(strings.ToLower(command))
	for i, word := range words {
		if filepath.Base(word) != "sed" {
			continue
		}
		for _, arg := range words[i+1:] {
			if arg == "-i" || strings.HasPrefix(arg, "-i.") || arg == "--in-place" || strings.HasPrefix(arg, "--in-place=") {
				return true
			}
		}
	}

	return false
}

func containsUnquotedShellRedirection(command string) bool {
	inSingle := false
	inDouble := false
	escaped := false

	for _, r := range command {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if !inSingle && !inDouble && (r == '>' || r == '<') {
			return true
		}
	}

	return false
}

func hasShellCommandSubstitution(command string) bool {
	return len(extractCommandSubstitutions(command)) > 0
}

func hasDangerousCommandSubstitution(command string) bool {
	for _, sub := range extractCommandSubstitutions(command) {
		if IsDangerousCommand(sub) {
			return true
		}
	}
	return false
}

func extractCommandSubstitutions(command string) []string {
	var substitutions []string

	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(command); i++ {
		ch := command[i]

		if escaped {
			escaped = false
			continue
		}

		if ch == '\\' && !inSingle {
			escaped = true
			continue
		}

		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}

		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}

		if inSingle {
			continue
		}

		if ch == '$' && i+1 < len(command) && command[i+1] == '(' {
			if sub, end, ok := readParenSubstitution(command, i+2); ok {
				substitutions = append(substitutions, sub)
				i = end
			}
			continue
		}

		if ch == '`' {
			if sub, end, ok := readBacktickSubstitution(command, i+1); ok {
				substitutions = append(substitutions, sub)
				i = end
			}
		}
	}

	return substitutions
}

func readParenSubstitution(command string, start int) (string, int, bool) {
	depth := 1
	inSingle := false
	inDouble := false
	escaped := false

	for i := start; i < len(command); i++ {
		ch := command[i]

		if escaped {
			escaped = false
			continue
		}

		if ch == '\\' && !inSingle {
			escaped = true
			continue
		}

		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}

		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}

		if inSingle || inDouble {
			continue
		}

		if ch == '(' {
			depth++
			continue
		}

		if ch == ')' {
			depth--
			if depth == 0 {
				return command[start:i], i, true
			}
		}
	}

	return "", 0, false
}

func readBacktickSubstitution(command string, start int) (string, int, bool) {
	escaped := false

	for i := start; i < len(command); i++ {
		ch := command[i]

		if escaped {
			escaped = false
			continue
		}

		if ch == '\\' {
			escaped = true
			continue
		}

		if ch == '`' {
			return command[start:i], i, true
		}
	}

	return "", 0, false
}

// splitCommandSegments splits a command string on |, &&, ||, ;, and newline
// boundaries. It respects single- and double-quoted strings.
func splitCommandSegments(command string) []string {
	var segments []string
	var current strings.Builder

	inSingle := false
	inDouble := false
	i := 0

	flush := func() {
		seg := strings.TrimSpace(current.String())
		if seg != "" {
			segments = append(segments, seg)
		}
		current.Reset()
	}

	for i < len(command) {
		ch := command[i]

		if ch == '\\' && i+1 < len(command) && !inSingle {
			current.WriteByte(ch)
			i++
			current.WriteByte(command[i])
			i++
			continue
		}

		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			current.WriteByte(ch)
			i++
			continue
		}

		if ch == '"' && !inSingle {
			inDouble = !inDouble
			current.WriteByte(ch)
			i++
			continue
		}

		if inSingle || inDouble {
			current.WriteByte(ch)
			i++
			continue
		}

		if i+1 < len(command) && ((ch == '&' && command[i+1] == '&') || (ch == '|' && command[i+1] == '|')) {
			flush()
			i += 2
			continue
		}

		// A lone `&` backgrounds the preceding command and starts a new one;
		// treat it as a separator so a destructive command after it isn't
		// hidden inside the previous segment. Redirections like `>&`/`<&`/`&>`
		// are handled by the redirection check, but here a bare `&` not part
		// of `&&` (already handled above) is a command separator.
		if ch == '&' {
			var prev byte
			if i > 0 {
				prev = command[i-1]
			}
			next := byte(0)
			if i+1 < len(command) {
				next = command[i+1]
			}
			if prev != '>' && prev != '<' && next != '>' {
				flush()
				i++
				continue
			}
		}

		if ch == '|' || ch == ';' || ch == '\n' {
			flush()
			i++
			continue
		}

		current.WriteByte(ch)
		i++
	}

	flush()

	return segments
}

// commandRunners are commands that take another command as their operands
// (e.g. `env rm -rf x`, `timeout 5 rm -rf x`). The classifier must look past
// the runner to the wrapped command, otherwise a destructive command can be
// hidden behind a benign-looking runner and skip the confirmation prompt.
var commandRunners = map[string]bool{
	"env":     true,
	"xargs":   true,
	"timeout": true,
	"nice":    true,
	"command": true,
	"time":    true,
	"nohup":   true,
	"stdbuf":  true,
	"setsid":  true,
	"ionice":  true,
	"taskset": true,
	"setarch": true,
}

// unwrapCommandWords peels leading VAR=val assignments, a leading backslash on
// the command name, and known runner prefixes (with their own flags/operands)
// so classification inspects the real command rather than the wrapper. It
// returns the resolved words, the lower-cased base command name, and whether a
// runner prefix consumed the entire input without revealing a real command
// (unresolved, which callers must treat conservatively).
func unwrapCommandWords(words []string) (resolved []string, cmd string, unresolved bool) {
	for {
		// Strip leading VAR=value environment assignments.
		for len(words) > 0 && isEnvAssignment(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 {
			return nil, "", true
		}

		// A leading backslash bypasses alias/function lookup in the shell but
		// still runs the underlying command (e.g. `\rm`).
		name := strings.TrimPrefix(words[0], `\`)
		base := strings.ToLower(filepath.Base(name))

		if !commandRunners[base] {
			return words, base, false
		}

		// Skip the runner's own flags and operands to reach the wrapped
		// command. For env/nice/timeout/etc. this means skipping leading
		// `-flag`/`-flag value` tokens (and, for env, further VAR=val pairs).
		rest := words[1:]
		rest = skipRunnerFlags(base, rest)
		if len(rest) == 0 {
			return nil, base, true
		}
		words = rest
	}
}

// skipRunnerFlags advances past a runner's own flag arguments so the next
// token is the wrapped command. It is deliberately conservative: an unknown
// flag that consumes a value would mis-align, so when in doubt we stop at the
// first non-flag token and let the caller classify it.
func skipRunnerFlags(runner string, args []string) []string {
	for len(args) > 0 {
		arg := args[0]
		if !strings.HasPrefix(arg, "-") {
			break
		}
		// `--` ends option parsing; the wrapped command follows.
		if arg == "--" {
			return args[1:]
		}
		// Flags that take a separate value for common runners.
		switch runner {
		case "timeout":
			// timeout [OPTION] DURATION COMMAND — `-s SIG`/`-k DURATION` take a value.
			if arg == "-s" || arg == "--signal" || arg == "-k" || arg == "--kill-after" {
				args = args[2:]
				continue
			}
		case "nice", "ionice":
			if arg == "-n" || arg == "--adjustment" || arg == "-c" || arg == "-p" {
				args = args[2:]
				continue
			}
		case "env":
			if arg == "-u" || arg == "--unset" {
				args = args[2:]
				continue
			}
		case "stdbuf":
			if arg == "-i" || arg == "-o" || arg == "-e" {
				args = args[2:]
				continue
			}
		case "taskset", "setarch":
			args = args[1:]
			continue
		}
		args = args[1:]
	}
	// env allows VAR=val pairs after its flags and before the command.
	if runner == "env" {
		for len(args) > 0 && isEnvAssignment(args[0]) {
			args = args[1:]
		}
	}
	// timeout's first non-flag operand is the DURATION, not the command.
	if runner == "timeout" && len(args) > 0 {
		args = args[1:]
	}
	return args
}

func isEnvAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	name := word[:eq]
	for i, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func isSingleCommandReadOnly(command string) bool {
	command = strings.TrimSpace(command)

	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}

	words, cmd, unresolved := unwrapCommandWords(fields)
	if unresolved {
		return false
	}

	subs, ok := normalizedReadOnlyCommands[cmd]
	if !ok {
		return false
	}

	args := words[1:]

	// Per-command argument blocklist for tools that are read-only by default
	// but have specific flags that escape the sandbox.
	switch cmd {
	case "find":
		for _, arg := range args {
			switch arg {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fls", "-fprint", "-fprint0", "-fprintf":
				return false
			}
		}
	case "sort":
		// `sort -o FILE` / `sort --output=FILE` writes to FILE.
		for _, arg := range args {
			if arg == "-o" || arg == "--output" || strings.HasPrefix(arg, "-o") || strings.HasPrefix(arg, "--output=") {
				return false
			}
		}
	case "jq", "yq", "xq":
		// `-i`/`--in-place` edits files in place.
		for _, arg := range args {
			if arg == "-i" || arg == "--in-place" || arg == "--inplace" {
				return false
			}
		}
	case "rg":
		for _, arg := range args {
			if arg == "-z" || arg == "--search-zip" ||
				arg == "--pre" || strings.HasPrefix(arg, "--pre=") ||
				arg == "--hostname-bin" || strings.HasPrefix(arg, "--hostname-bin=") {
				return false
			}
		}
	case "git":
		if hasUnsafeGitOptions(args) {
			return false
		}
	}

	if len(subs) == 0 {
		return true
	}

	if len(args) == 0 {
		return false
	}

	rest := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(command, words[0])))
	for _, sub := range subs {
		if hasSubcommandPrefix(rest, sub) {
			return true
		}
	}

	return false
}

func hasSubcommandPrefix(command, prefix string) bool {
	if !strings.HasPrefix(command, prefix) {
		return false
	}
	if len(command) == len(prefix) {
		return true
	}
	return command[len(prefix)] == ' '
}

// hasUnsafeGitOptions blocks global git options that can redirect config
// or repo lookup (and thus run arbitrary code via hooks/aliases).
func hasUnsafeGitOptions(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "-C", "-c", "--config-env", "--exec-path", "--git-dir", "--namespace", "--super-prefix", "--work-tree",
			"--output", "--ext-diff", "--textconv", "--exec", "--paginate":
			return true
		}
		if strings.HasPrefix(arg, "-C") && arg != "-C" {
			return true
		}
		if strings.HasPrefix(arg, "-c") && arg != "-c" {
			return true
		}
		if strings.HasPrefix(arg, "--config-env=") ||
			strings.HasPrefix(arg, "--exec-path=") ||
			strings.HasPrefix(arg, "--git-dir=") ||
			strings.HasPrefix(arg, "--namespace=") ||
			strings.HasPrefix(arg, "--super-prefix=") ||
			strings.HasPrefix(arg, "--work-tree=") ||
			strings.HasPrefix(arg, "--output=") ||
			strings.HasPrefix(arg, "--exec=") {
			return true
		}
	}

	return false
}

func isDangerousSingleCommand(command string) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return false
	}

	words, cmd, unresolved := unwrapCommandWords(fields)
	if unresolved {
		// A runner prefix (e.g. `env`, `xargs`) with no resolvable wrapped
		// command — treat as dangerous so it can't slip past the prompt.
		return true
	}
	args := words[1:]

	switch cmd {
	case "sudo", "su", "doas":
		return true
	case "dd", "mkfs", "mount", "umount", "diskutil", "launchctl", "systemctl", "service":
		return true
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return isDangerousPowerShellInvocation(args)
	case "cmd", "cmd.exe":
		return IsDangerousCommand(extractCmdScript(args))
	case "remove-item", "ri":
		return hasPowerShellForceOrRecursive(args)
	case "stop-process":
		return hasAnyArgFold(args, "-force")
	case "invoke-expression", "iex", "set-executionpolicy", "new-service", "sc.exe", "reg", "reg.exe":
		return true
	case "del", "erase":
		return hasAnyArgFold(args, "/f")
	case "rd", "rmdir":
		return hasAnyArgFold(args, "/s")
	case "start", "explorer", "explorer.exe", "mshta", "mshta.exe":
		return argsHaveURL(args)
	case "rundll32", "rundll32.exe":
		return argsHaveURL(args) && containsArgFold(args, "url.dll,fileprotocolhandler")
	case "rm":
		return hasRecursiveRemoveArg(args)
	case "git":
		return isDangerousGitCommand(args)
	}

	return false
}

func extractCmdScript(args []string) string {
	for i, arg := range args {
		switch strings.ToLower(strings.Trim(arg, `"'`)) {
		case "/c", "/r", "-c":
			if i+1 < len(args) {
				return trimOuterQuotes(strings.Join(args[i+1:], " "))
			}
			return ""
		}
	}

	return ""
}

func isDangerousPowerShellInvocation(args []string) bool {
	for _, arg := range args {
		switch strings.ToLower(strings.Trim(arg, `"'`)) {
		case "-encodedcommand", "-ec", "-e", "-file", "/file", "-executionpolicy":
			return true
		}
	}

	return isDangerousPowerShellScript(extractPowerShellScript(args))
}

func extractPowerShellScript(args []string) string {
	for i, arg := range args {
		lower := strings.ToLower(strings.Trim(arg, `"'`))
		switch lower {
		case "-command", "/command", "-c":
			if i+1 < len(args) {
				return trimOuterQuotes(strings.Join(args[i+1:], " "))
			}
			return ""
		}
		if strings.HasPrefix(lower, "-command:") || strings.HasPrefix(lower, "/command:") {
			return trimOuterQuotes(arg[strings.Index(arg, ":")+1:])
		}
		if !strings.HasPrefix(lower, "-") && !strings.HasPrefix(lower, "/") {
			return trimOuterQuotes(strings.Join(args[i:], " "))
		}
	}

	return ""
}

func trimOuterQuotes(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}

	first := value[0]
	last := value[len(value)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return value[1 : len(value)-1]
	}

	return value
}

func isDangerousPowerShellScript(script string) bool {
	if strings.TrimSpace(script) == "" {
		return false
	}

	segments := splitCommandSegments(script)
	for i, segment := range segments {
		words := strings.Fields(segment)
		if len(words) == 0 {
			continue
		}

		cmd := strings.ToLower(strings.Trim(words[0], `"'(){}[]`))
		args := words[1:]

		switch cmd {
		case "remove-item", "ri", "rm", "del", "erase", "rd", "rmdir":
			if hasPowerShellForceOrRecursive(args) {
				return true
			}
		case "stop-process":
			if hasAnyArgFold(args, "-force") {
				return true
			}
		case "invoke-expression", "iex", "set-executionpolicy", "new-service", "sc.exe", "reg", "reg.exe":
			return true
		case "start-process", "start", "saps", "invoke-item", "ii", "explorer", "explorer.exe", "mshta", "mshta.exe":
			if argsHaveURL(args) {
				return true
			}
		case "rundll32", "rundll32.exe":
			if argsHaveURL(args) && containsArgFold(args, "url.dll,fileprotocolhandler") {
				return true
			}
		}

		if i > 0 && (cmd == "invoke-expression" || cmd == "iex") && isPowerShellDownloadCommand(segments[i-1]) {
			return true
		}
	}

	return false
}

func hasPowerShellForceOrRecursive(args []string) bool {
	return hasAnyArgFold(args, "-force", "-recurse", "-recursive") ||
		hasAnyArgPrefixFold(args, "-force:", "-recurse:", "-recursive:")
}

func isPowerShellDownloadCommand(command string) bool {
	words := strings.Fields(strings.TrimSpace(command))
	if len(words) == 0 {
		return false
	}
	switch strings.ToLower(strings.Trim(words[0], `"'(){}[]`)) {
	case "invoke-webrequest", "iwr", "curl", "wget":
		return true
	}
	return false
}

func isDangerousGitCommand(args []string) bool {
	switch firstNonFlagArg(args) {
	case "clean":
		return true
	case "reset":
		return hasAnyArg(args, "--hard") || hasAnyArgPrefix(args, "--hard=")
	case "checkout":
		return hasAnyArg(args, "-f", "--force")
	case "push":
		return hasAnyArg(args, "--force", "--force-with-lease", "-f") ||
			hasAnyArgPrefix(args, "--force-with-lease=")
	case "branch":
		return hasAnyArg(args, "-D")
	}

	return false
}

func hasRecursiveRemoveArg(args []string) bool {
	for _, arg := range args {
		if arg == "--recursive" || strings.HasPrefix(arg, "--recursive=") {
			return true
		}

		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			continue
		}

		flags := strings.TrimLeft(arg, "-")
		if strings.ContainsAny(flags, "rR") {
			return true
		}
	}

	return false
}

func firstNonFlagArg(args []string) string {
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch arg {
		case "-C", "-c", "--config-env", "--exec-path", "--git-dir", "--namespace", "--super-prefix", "--work-tree":
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}

	return ""
}

func isDownloadCommand(command string) bool {
	words := strings.Fields(strings.TrimSpace(command))
	if len(words) == 0 {
		return false
	}
	switch strings.ToLower(filepath.Base(words[0])) {
	case "curl", "wget":
		return true
	}
	return false
}

func isShellInterpreter(command string) bool {
	words := strings.Fields(strings.TrimSpace(command))
	if len(words) == 0 {
		return false
	}
	switch strings.ToLower(filepath.Base(words[0])) {
	case "sh", "bash", "zsh", "fish":
		return true
	}
	return false
}

func hasAnyArg(args []string, values ...string) bool {
	for _, arg := range args {
		for _, value := range values {
			if arg == value {
				return true
			}
		}
	}
	return false
}

func hasAnyArgFold(args []string, values ...string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.Trim(arg, `"'`))
		for _, value := range values {
			if arg == strings.ToLower(value) {
				return true
			}
		}
	}
	return false
}

func hasAnyArgPrefix(args []string, prefixes ...string) bool {
	for _, arg := range args {
		for _, prefix := range prefixes {
			if strings.HasPrefix(arg, prefix) {
				return true
			}
		}
	}
	return false
}

func hasAnyArgPrefixFold(args []string, prefixes ...string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.Trim(arg, `"'`))
		for _, prefix := range prefixes {
			if strings.HasPrefix(arg, strings.ToLower(prefix)) {
				return true
			}
		}
	}
	return false
}

func containsArgFold(args []string, needle string) bool {
	needle = strings.ToLower(needle)
	for _, arg := range args {
		if strings.Contains(strings.ToLower(strings.Trim(arg, `"'`)), needle) {
			return true
		}
	}
	return false
}

func argsHaveURL(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.Trim(arg, `"'`))
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return true
		}
	}
	return false
}
