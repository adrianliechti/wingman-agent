package shell

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Compares the classifier with PowerShell's own parser over generated command shapes; nothing is executed.
// WINGMAN_PWSH=docker go test ./pkg/harness/tool/shell -run '^TestPowerShellParserOracle$' -count=1
// WINGMAN_PWSH may also be docker:IMAGE or the path of a pwsh binary. Run it after updating gotreesitter.
func TestPowerShellParserOracle(t *testing.T) {
	runner := os.Getenv("WINGMAN_PWSH")
	if runner == "" {
		t.Skip("set WINGMAN_PWSH to docker or a pwsh binary to compare with PowerShell's parser")
	}
	corpus := powerShellOracleCorpus()
	dir := t.TempDir()
	data, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corpus.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oracle.ps1"), []byte(powerShellOracleScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(runner, "-NoLogo", "-NoProfile", "-File", filepath.Join(dir, "oracle.ps1"), filepath.Join(dir, "corpus.json"), filepath.Join(dir, "oracle.jsonl"))
	if image, docker := strings.CutPrefix(runner, "docker"); docker {
		if image = strings.TrimPrefix(image, ":"); image == "" {
			image = "mcr.microsoft.com/powershell:7.5-azurelinux-3.0"
			if runtime.GOARCH == "arm64" {
				image += "-arm64"
			}
		}
		cmd = exec.Command("docker", "run", "--rm", "--network", "none", "-v", dir+":/w", image, "pwsh", "-NoLogo", "-NoProfile", "-File", "/w/oracle.ps1", "/w/corpus.json", "/w/oracle.jsonl")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PowerShell did not run: %v\n%s", err, out)
	}
	file, err := os.Open(filepath.Join(dir, "oracle.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	problems := map[string][]string{}
	report := func(kind, command string) { problems[kind] = append(problems[kind], command) }
	lines := bufio.NewScanner(file)
	lines.Buffer(nil, 1<<20)
	checked := 0
	for _, command := range corpus {
		if !lines.Scan() {
			t.Fatalf("PowerShell parsed %d of %d commands", checked, len(corpus))
		}
		var parsed struct {
			Errors       int      `json:"errors"`
			Commands     []string `json:"commands"`
			Redirections []string `json:"redirections"`
			Calls        int      `json:"calls"`
		}
		if err := json.Unmarshal(lines.Bytes(), &parsed); err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		checked++
		analysis := analyzeShellCommand(command, dialectPowerShell)
		// A command PowerShell refuses runs nothing, and one sent to review hides nothing from the reviewer.
		if parsed.Errors > 0 || isDangerousShellAnalysis(analysis, dialectPowerShell) {
			continue
		}
		seen := map[string]bool{}
		for _, statement := range analysis.syntax.statements {
			statement = strings.TrimSpace(statement)
			if rest, ok := strings.CutPrefix(statement, "& "); ok {
				statement = rest
			} else if rest, ok := strings.CutPrefix(statement, ". "); ok {
				statement = rest
			}
			if fields := strings.Fields(statement); len(fields) > 0 {
				seen[strings.ToLower(fields[0])] = true
			}
		}
		for _, name := range parsed.Commands {
			// A name that is itself an expression is found through the expression.
			if name != "" && !strings.ContainsAny(name[:1], "{($\"'") && !seen[strings.ToLower(name)] {
				report("PowerShell runs a command the classifier does not see", command)
				break
			}
		}
		for _, target := range parsed.Redirections {
			if target != "$null" && !analysis.syntax.hasRedirection {
				report("PowerShell redirects to a file the classifier does not see", command)
				break
			}
		}
		if parsed.Calls > 0 && isReadOnlyShellAnalysis(analysis) {
			report("a method call is judged read-only", command)
		}
	}
	for kind, commands := range problems {
		t.Errorf("%s: %d of %d commands, for example %q", kind, len(commands), checked, commands[:min(len(commands), 8)])
	}
}

const powerShellOracleScript = `param([string]$In, [string]$Out)
$parser = [System.Management.Automation.Language.Parser]
$writer = [System.IO.StreamWriter]::new($Out)
foreach ($text in (Get-Content $In -Raw | ConvertFrom-Json)) {
  $tokens = $null; $errors = $null
  $ast = $parser::ParseInput($text, [ref]$tokens, [ref]$errors)
  $commands = @(); $redirections = @(); $calls = 0
  foreach ($node in $ast.FindAll({ $true }, $true)) {
    switch ($node.GetType().Name) {
      'CommandAst' { $commands += $node.CommandElements[0].Extent.Text }
      'FileRedirectionAst' { $redirections += $node.Location.Extent.Text }
      'InvokeMemberExpressionAst' { $calls++ }
    }
  }
  $writer.WriteLine((@{ errors = $errors.Count; commands = $commands; redirections = $redirections; calls = $calls } | ConvertTo-Json -Compress))
}
$writer.Close()
`

// Arguments built from every prefix, joiner and value, tokens glued to one another, and whole-statement shapes.
func powerShellOracleCorpus() []string {
	heads := []string{"tool", "git log"}
	tails := []string{
		"", " tail", "; Get-Item .", " | Get-Item", " > out.txt", "\nGet-Item .",
	}
	prefixes := []string{"-x", "--x", "x", "-Path", "/p:x", ""}
	joiners := []string{"=", ":", "", " "}
	values := []string{
		"\"$(Get-Date)\"", "'$(Get-Date)'", "\"a b\"", "'a b'", "\"a\"\"$(Get-Date)\"", "a\"$(Get-Date)\"", "\"$y\"",
		"\"${y}\"", "\"a$(Get-Date)b\"c", "$(Get-Date)", "@(Get-Date)", "(Get-Date)", "{Get-Date}", "a(Get-Date)",
		"a{Get-Date}", "$y", "${y}", "$env:A", "@args", "$y.Foo()", "[IO.File]::Delete('x')", "a>b", ">b", ">>b", "2>b",
		"*>b", "5>b", "1>b", "7>b", "a2>b", "<b", "a<b", "2>&1", "\"a\">b", "'a'>b", "(1)>b", ">\"b\"", ">'b'", ">$null",
		"2>$null", "a,b", "a;Get-Date", "a|Get-Date", "a&Get-Date", "a&&Get-Date", "a||Get-Date", "`x", "a`nb", "`\"q`\"",
		"a`;b", "`$(Get-Date)", "a` b", "`-y", "`>b", "a`>b", "a#b", "#b", "*.go", "C:\\a\\b", "a!b", "%h", "~/x", "..",
		"-", "--", "--%", "1", "-5", "\"a>b\"", "'a>b'", "\"a;Get-Date\"", "\"a|Get-Date\"", "\"a`\"$(Get-Date)\"",
		"\"a'$(Get-Date)'\"", "'a\"$(Get-Date)\"'", "\"a`$(Get-Date)\"",
	}
	firsts := []string{
		"\"a\"", "'a'", "$y", "(1)", "@args", "1", "a.b", "-x", "--x", "x", "-x:", "-x=", "--x=", "x=", "/p:x=",
	}
	glues := []string{
		"#b", ">b", ">>b", "2>b", "<b", "(Get-Date)", "$(Get-Date)", "@(Get-Date)", "{Get-Date}", "\"$(Get-Date)\"", "'c'",
		"\"c\"", "$y", ",b", ".Foo()", "::Foo()", "[0]", "`x", "`nb", " --%", " --% z", "--%", "@{a=Get-Date}",
		"@{a=(Get-Date)}", "!b", "%b", "*", "?", "~", "\\b", "/b", ":b", "=b", "+b", "-b", "|Get-Date", ";Get-Date",
		"&Get-Date", " #b", " <#b#>", "<#b#>",
	}
	shapes := []string{
		"& tool x", "& \"tool\" x", "& $cmd x", ". ./script.ps1", "& { Get-Date }", "tool x; & { Get-Date }",
		"$a = tool x", "$a = $(Get-Date)", "$a = Get-Date", "$a = (Get-Date)", "if ($true) { Get-Date }",
		"if (Get-Date) { tool x }", "foreach ($i in 1..3) { Get-Date }", "while ($false) { Get-Date }",
		"try { Get-Date } catch { Get-Item . }", "try { tool x } finally { Get-Date }", "switch (1) { 1 { Get-Date } }",
		"function f { Get-Date }", "function f { Get-Date }; f", "filter g { Get-Date }", "$sb = { Get-Date }",
		"$sb = { Get-Date }; & $sb", ". { Get-Date }", "1..3 | ForEach-Object { Get-Date }", "1..3 | % { Get-Date }",
		"tool x | ? { Get-Date }", "tool x | Where-Object { Get-Date }", "Invoke-Command { Get-Date }",
		"Invoke-Command -ScriptBlock { Get-Date }", "Start-Job { Get-Date }", "Measure-Command { Get-Date }",
		"tool x | ForEach-Object -Process { Get-Date }", "tool @params", "tool @params; Get-Date", "& \"$(Get-Date)\"",
		"& (Get-Date)", "& $(Get-Date)", "tool \"$(Get-Date)\"", "tool \"a $(Get-Date) b\"", "tool '$(Get-Date)'",
		"tool $(Get-Date)", "tool (Get-Date)", "tool @(Get-Date)", "tool { Get-Date }", "tool -x (Get-Date)",
		"tool -x:(Get-Date)", "@\"\n$(Get-Date)\n\"@", "tool @\"\n$(Get-Date)\n\"@", "tool @'\n$(Get-Date)\n'@",
		"$x = @\"\n$(Get-Date)\n\"@", "tool x `\n  -y", "tool x `\n  ; Get-Date", "tool x\nGet-Date",
		"tool x # c\nGet-Date", "tool x # c; Get-Date", "tool x <# c #> ; Get-Date", "tool x <# c\nGet-Date #>",
		"# c\nGet-Date", "tool x && Get-Date", "tool x || Get-Date", "tool x & Get-Date", "tool x; Get-Date",
		"tool x | Get-Date", "tool x |\n Get-Date", "Get-Date | tool x", "Get-Date; tool x", "$y.Foo()",
		"[IO.File]::Delete(\"x\")", "tool ([IO.File]::ReadAllText(\"x\"))", "tool $y.Foo()", "tool \"$($y.Foo())\"",
		"tool ($y).Foo()", "$y::Foo()", "$env:A = \"b\"; tool x", "$env:PATH = \"x\"", "${env:A} = 1", "tool $env:A",
		"tool ${env:A}", "tool $env:A.Length", "tool x > out.txt", "tool x >> out.txt", "tool x 2> err.txt", "tool x 2>&1",
		"tool x *> out.txt", "tool x > $null", "tool x | Out-File out.txt", "tool x | Set-Content out.txt",
		"tool x | Tee-Object out.txt", "Get-Date > out.txt", "(Get-Date) > out.txt", "$(Get-Date) > out.txt",
		"& { Get-Date } > out.txt", "tool x 3>&1 2> err.txt", "tool -x --% y", "tool --% y | Get-Date",
		"tool --% y ; Get-Date", "tool --% y\nGet-Date", "tool 'a' 'b'", "tool \"a\" \"b\"", "tool \"a\"\"b\"",
		"tool 'a''b'", "tool \"a`\"b\"", "tool \"a`$(Get-Date)\"", "tool \"a`n$(Get-Date)\"", "tool 'a`'", "tool `\n",
		"tool x `", "tool \"unterminated", "tool 'unterminated", "tool (", "tool {", "tool )", "tool }", "tool |",
		"tool ;", ";tool x", "| tool x",
	}
	seen := map[string]bool{}
	var corpus []string
	add := func(command string) {
		if !seen[command] {
			seen[command] = true
			corpus = append(corpus, command)
		}
	}
	for _, head := range heads {
		for _, tail := range tails {
			for _, prefix := range prefixes {
				for _, joiner := range joiners {
					if prefix == "" && joiner != "" {
						continue
					}
					for _, value := range values {
						add(head + " " + prefix + joiner + value + tail)
					}
				}
			}
			for _, first := range firsts {
				for _, glue := range glues {
					add(head + " " + first + glue + tail)
				}
			}
		}
	}
	for _, shape := range shapes {
		add(shape)
		add("git status; " + shape)
		add(shape + "; git status")
	}
	return corpus
}
