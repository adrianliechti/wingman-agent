package shell

import "testing"

// Ported from codex's is_dangerous_command tests: forced removals stay
// dangerous through absolute paths, env assignments, deep wrapper nesting,
// substitutions and nested shells, while quoted or trap-echoed spellings that
// never execute rm stay quiet.
func TestClassifierCodexForcedRemoveSpellingsAndCounterexamples(t *testing.T) {
	dangerous := []string{
		"/bin/rm -fr /tmp/x",
		"rm -r -f /tmp/x",
		"env TARGET=x rm -rf /tmp/x",
		"env env env env env env env env rm -rf /tmp/x",
		"env env env env env env env env env rm -rf /tmp/x", // deeper than codex inspects: must fail closed
		`echo "$(rm -rf /tmp/x)"`,
		"bash -c 'rm -rf /tmp/x'",
		"zsh -lc 'rm -rf /tmp/x'",
	}
	for _, command := range dangerous {
		if !isDangerousCommandDialect(command, dialectBash) {
			t.Errorf("%q must be dangerous", command)
		}
	}
	quiet := []string{
		"rm -- -f",
		"echo 'rm -rf /tmp/x'",
		"trap 'echo rm -rf /tmp/x' EXIT",
	}
	for _, command := range quiet {
		if isDangerousCommandDialect(command, dialectBash) {
			t.Errorf("%q must not be dangerous", command)
		}
	}
}

// Ported from codex's windows_dangerous_commands tests, run against the
// PowerShell dialect directly so the table is checked on every platform.
func TestClassifierCodexWindowsDangerousCommands(t *testing.T) {
	dangerous := []string{
		"Start-Process 'https://example.com'",
		"Start-Process 'hTtPs://example.com';",
		"cmd /c start https://example.com",
		`start "" https://example.com`,
		"Remove-Item test -Force",
		"Remove-Item -Recurse -Force test",
		"Remove-Item -Path 'test' -Force",
		"ri test -Force",
		"rm test -Force",
		"Remove-Item test -Force; Write-Host done",
		"if ($true) { Remove-Item test -Force }",
		"Write-Host hi;Remove-Item -Force C:\\tmp",
		"del,-Force,C:\\foo",
		"del /f file.txt",
		"erase /f file.txt",
		"DEL /F file.txt",
		"rd /s /q d",
		"rmdir /s /q d",
		"cmd.exe /r del /f file.txt",
		"echo hi&rmdir /s /q d",
		"echo hi && del /f x",
		"echo hi || del /f x",
	}
	for _, command := range dangerous {
		if !isDangerousCommandDialect(command, dialectPowerShell) {
			t.Errorf("%q must be dangerous", command)
		}
	}
	quiet := []string{
		"Start-Process notepad.exe",
		"explorer.exe .",
		"Remove-Item test",
		"Get-ChildItem -Force; Remove-Item test",
		"del test.txt",
		"del C:/foo/bar.txt", // an f inside a path is not a flag
		"rd C:/source",
		"echo del /f",
	}
	for _, command := range quiet {
		if isDangerousCommandDialect(command, dialectPowerShell) {
			t.Errorf("%q must not be dangerous", command)
		}
	}
}

// Ported from codex's historical is_safe_command tests: the known-safe
// allowlist and the flags that take a tool out of read-only territory.
func TestClassifierCodexKnownSafeCommands(t *testing.T) {
	safe := []string{
		"cat f", "cut -d, -f1 f", "echo hi", "expr 1 + 1", "grep -n foo f", "head -n 10 f", "id", "ls -la",
		"nl f", "paste a b", "pwd", "rev f", "seq 3", "stat f", "tail -n 5 f", "tr a-z A-Z", "uname -a",
		"uniq f", "wc -l f", "which go", "whoami", "tac f",
		"base64 -d in", "sed -n '3p' f", "sed -n '2,4p' f", "find . -name file.txt",
		"grep -R Cargo.toml -n", "cat f | sed -n '1,200p'", "ls | wc -l", "echo 'hi' ; ls", "(ls)",
	}
	for _, command := range safe {
		if !isReadOnlyCommandDialect(command, dialectBash) {
			t.Errorf("%q must be read-only", command)
		}
	}
	unsafe := []string{
		`find . -delete`, `find . -exec rm {} \;`, `find . -execdir ls {} \;`, `find . -ok rm {} \;`, `find . -okdir rm {} \;`,
		"find . -fprint out", "find . -fls out", "find . -fprintf out %p", "find . -fprint0 out",
		"rg --pre cat x", "rg --pre=cat x", "rg --search-zip x", "rg -z x", "rg --hostname-bin=x y", "rg --hostname-bin x y",
		"base64 -o out in", "base64 -oout in", "base64 --output=x in", "base64 --output x in",
		"sed -n xp f", "sed -i s/a/b/ f", "sed s/a/b/ f",
		"ls > out.txt", "echo hi | tee out", "cargo check", "bash -lc 'ls && rm -rf /'",
	}
	for _, command := range unsafe {
		if isReadOnlyCommandDialect(command, dialectBash) {
			t.Errorf("%q must not be read-only", command)
		}
	}
}
