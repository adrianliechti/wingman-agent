package code

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	userShellOpenTag    = "<user_shell_command>"
	userShellCloseTag   = "</user_shell_command>"
	userShellCommandTag = "<command>"
	userShellCmdEndTag  = "</command>"
	userShellResultTag  = "<result>"
	userShellResEndTag  = "</result>"
)

// FormatUserShellCommand formats a user-executed `!` shell command and its
// result as a structured conversation fragment (matching Codex's
// <user_shell_command> schema) so the agent has the command and output in
// context.
func FormatUserShellCommand(command, output string, exitCode int, duration time.Duration) string {
	command = strings.TrimSpace(command)
	output = strings.TrimRight(output, "\r\n")
	seconds := duration.Seconds()

	var b strings.Builder
	b.WriteString(userShellOpenTag)
	b.WriteString("\n")
	b.WriteString(userShellCommandTag)
	b.WriteString("\n")
	b.WriteString(command)
	b.WriteString("\n")
	b.WriteString(userShellCmdEndTag)
	b.WriteString("\n")
	b.WriteString(userShellResultTag)
	b.WriteString("\n")
	fmt.Fprintf(&b, "Exit code: %d\n", exitCode)
	fmt.Fprintf(&b, "Duration: %.4f seconds\n", seconds)
	b.WriteString("Output:\n")
	if output != "" {
		b.WriteString(output)
		b.WriteString("\n")
	}
	b.WriteString(userShellResEndTag)
	b.WriteString("\n")
	b.WriteString(userShellCloseTag)
	return b.String()
}

// IsUserShellCommand reports whether text is a formatted <user_shell_command>
// conversation record.
func IsUserShellCommand(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, userShellOpenTag) && strings.HasSuffix(trimmed, userShellCloseTag)
}

// ParseUserShellCommand extracts the command, output body, and exit code from
// a <user_shell_command> record.
func ParseUserShellCommand(text string) (command string, output string, exitCode int, ok bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, userShellOpenTag) || !strings.HasSuffix(trimmed, userShellCloseTag) {
		return "", "", 0, false
	}

	cmdStart := strings.Index(trimmed, userShellCommandTag)
	cmdEnd := strings.Index(trimmed, userShellCmdEndTag)
	resStart := strings.Index(trimmed, userShellResultTag)
	resEnd := strings.LastIndex(trimmed, userShellResEndTag)
	if cmdStart < 0 || cmdEnd <= cmdStart || resStart <= cmdEnd || resEnd <= resStart {
		return "", "", 0, false
	}

	command = strings.TrimSpace(trimmed[cmdStart+len(userShellCommandTag) : cmdEnd])
	result := strings.Trim(trimmed[resStart+len(userShellResultTag):resEnd], "\r\n")

	for line := range strings.SplitSeq(result, "\n") {
		if after, found := strings.CutPrefix(strings.TrimSpace(line), "Exit code:"); found {
			if code, err := strconv.Atoi(strings.TrimSpace(after)); err == nil {
				exitCode = code
			}
			break
		}
	}

	if _, after, found := strings.Cut(result, "Output:\n"); found {
		output = strings.TrimRight(after, "\r\n")
	} else if strings.HasSuffix(result, "Output:") {
		output = ""
	} else {
		output = result
	}

	return command, output, exitCode, true
}
