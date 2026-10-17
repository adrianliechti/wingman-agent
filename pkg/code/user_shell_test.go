package code

import (
	"strings"
	"testing"
	"time"
)

func TestFormatAndParseUserShellCommand(t *testing.T) {
	formatted := FormatUserShellCommand("git status -s", " M pkg/tui/code/app.go\n", 0, 125*time.Millisecond)
	if !IsUserShellCommand(formatted) {
		t.Fatalf("expected IsUserShellCommand to be true for %q", formatted)
	}
	for _, want := range []string{
		"<user_shell_command>",
		"<command>\ngit status -s\n</command>",
		"Exit code: 0",
		"Output:\n M pkg/tui/code/app.go",
		"</user_shell_command>",
	} {
		if !strings.Contains(formatted, want) {
			t.Fatalf("formatted shell record missing %q:\n%s", want, formatted)
		}
	}

	cmd, output, exitCode, ok := ParseUserShellCommand(formatted)
	if !ok {
		t.Fatalf("ParseUserShellCommand failed on:\n%s", formatted)
	}
	if cmd != "git status -s" {
		t.Fatalf("cmd = %q, want %q", cmd, "git status -s")
	}
	if output != " M pkg/tui/code/app.go" {
		t.Fatalf("output = %q, want %q", output, " M pkg/tui/code/app.go")
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
}

func TestParseUserShellCommandNonZeroEmptyOutput(t *testing.T) {
	formatted := FormatUserShellCommand("false", "", 1, 10*time.Millisecond)
	cmd, output, exitCode, ok := ParseUserShellCommand(formatted)
	if !ok {
		t.Fatalf("ParseUserShellCommand failed on:\n%s", formatted)
	}
	if cmd != "false" || output != "" || exitCode != 1 {
		t.Fatalf("got (%q, %q, %d), want (\"false\", \"\", 1)", cmd, output, exitCode)
	}
	if IsUserShellCommand("normal user prompt") {
		t.Fatal("IsUserShellCommand matched normal user prompt")
	}
}
