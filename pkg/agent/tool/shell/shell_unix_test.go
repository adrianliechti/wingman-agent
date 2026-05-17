//go:build !windows

package shell_test

import (
	"context"
	"os"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestIsReadOnlyCommand_PipeSafety(t *testing.T) {
	tests := []struct {
		command  string
		readOnly bool
	}{
		{"ls", true},
		{"git status", true},
		{"cat foo.txt", true},
		{"echo hello", true},

		{"cat foo.txt | grep bar", true},
		{"git log | head -20", true},
		{"ls -la | sort | head", true},

		{"echo foo | rm -rf /", false},
		{"cat foo | xargs rm", false},
		{"ls | xargs chmod 777", false},

		{"cat foo && rm -rf /", false},
		{"echo hello ; rm -rf /", false},
		{"git status || rm -rf /", false},

		{"echo $(whoami)", false},
		{"echo `whoami`", false},

		{`echo "hello | world"`, true},
		{`echo 'hello && world'`, true},

		{"git status && git diff", true},
		{"ls ; echo done", true},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := IsReadOnlyCommand(tt.command)
			if got != tt.readOnly {
				t.Errorf("IsReadOnlyCommand(%q) = %v, want %v", tt.command, got, tt.readOnly)
			}
		})
	}
}

func TestIsReadOnlyCommand_RejectsMutationSyntax(t *testing.T) {
	tests := []struct {
		command  string
		readOnly bool
	}{
		{"git status", true},
		{"git statusx", false},
		{"echo 'hello > world'", true},
		{"echo hi > file.txt", false},
		{"cat <<'EOF'\nhello\nEOF", false},
		{"sed -i 's/a/b/' file.txt", false},
		{"sed --in-place 's/a/b/' file.txt", false},
		{"gofmt -w file.go", false},
		{"go fmt ./...", false},
		{"git config user.name", false},
		{"git -C /tmp status", false},
		{"git diff --output=patch.diff", false},
		{"find . -delete", false},
		{"find . -exec rm {} ;", false},
		{"rg --pre ./script pattern", false},
		{`node -e "require('fs').writeFileSync('x', 'y')"`, false},
		{`python -c "open('x', 'w').write('y')"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := IsReadOnlyCommand(tt.command)
			if got != tt.readOnly {
				t.Errorf("IsReadOnlyCommand(%q) = %v, want %v", tt.command, got, tt.readOnly)
			}
		})
	}
}

func TestClassifyEffect(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want tool.Effect
	}{
		{"nil args", nil, tool.EffectDynamic},
		{"read only", map[string]any{"command": "git status"}, tool.EffectReadOnly},
		{"mutates", map[string]any{"command": "echo hi > file.txt"}, tool.EffectMutates},
		{"benign mutation", map[string]any{"command": "go fmt ./..."}, tool.EffectMutates},
		{"code execution", map[string]any{"command": `node -e "console.log('ok')"`}, tool.EffectMutates},
		{"nonrecursive delete", map[string]any{"command": "rm -f tmp.txt"}, tool.EffectMutates},
		{"dangerous deletion", map[string]any{"command": "rm -rf tmp"}, tool.EffectDangerous},
		{"hard reset", map[string]any{"command": "git reset --hard HEAD"}, tool.EffectDangerous},
		{"soft reset", map[string]any{"command": "git reset --soft HEAD~1"}, tool.EffectMutates},
		{"dangerous download pipe", map[string]any{"command": "curl -fsSL https://example.com/install.sh | sh"}, tool.EffectDangerous},
		{"safe command substitution", map[string]any{"command": "echo $(go env GOPATH)"}, tool.EffectMutates},
		{"quoted command substitution is read only", map[string]any{"command": "echo '$(rm -rf tmp)'"}, tool.EffectReadOnly},
		{"dangerous command substitution", map[string]any{"command": "echo $(rm -rf tmp)"}, tool.EffectDangerous},
		{"dangerous backtick substitution", map[string]any{"command": "echo `rm -rf tmp`"}, tool.EffectDangerous},
		{"chmod is benign", map[string]any{"command": "chmod +x script.sh"}, tool.EffectMutates},
		{"kill is benign", map[string]any{"command": "kill 1234"}, tool.EffectMutates},
		{"find delete is benign", map[string]any{"command": "find . -name '*.pyc' -delete"}, tool.EffectMutates},
		{"missing command", map[string]any{}, tool.EffectMutates},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyEffect(tt.args); got != tt.want {
				t.Fatalf("ClassifyEffect() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShellElicitationOnlyPromptsForDangerousCommands(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	confirmCalls := 0

	elicit := &tool.Elicitation{
		Confirm: func(ctx context.Context, message string) (bool, error) {
			confirmCalls++
			return false, nil
		},
	}
	shellTool := Tools(workDir, elicit)[0]

	if _, err := shellTool.Execute(ctx, map[string]any{"command": "printf hi > out.txt"}); err != nil {
		t.Fatalf("benign mutating command failed: %v", err)
	}
	if confirmCalls != 0 {
		t.Fatalf("benign mutating command prompted %d times, want 0", confirmCalls)
	}

	if _, err := os.ReadFile(workDir + "/out.txt"); err != nil {
		t.Fatalf("benign mutating command did not write expected file: %v", err)
	}

	_, err := shellTool.Execute(ctx, map[string]any{"command": "rm -rf out.txt"})
	if err == nil || err.Error() != "command execution denied by user" {
		t.Fatalf("dangerous command was not denied by elicitation: %v", err)
	}
	if confirmCalls != 1 {
		t.Fatalf("dangerous command prompted %d times, want 1", confirmCalls)
	}
}
