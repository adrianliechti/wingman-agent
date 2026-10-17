//go:build windows

package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxSIDIsStablePerWorkspace(t *testing.T) {
	_, a, err := sandboxSID(`F:\src\one`)
	if err != nil {
		t.Fatal(err)
	}
	_, again, _ := sandboxSID(`f:\SRC\one\`)
	_, other, _ := sandboxSID(`F:\src\two`)
	if !a.Equals(again) || a.Equals(other) || !strings.HasPrefix(a.String(), "S-1-9-") {
		t.Fatalf("sandbox SIDs: %s %s %s", a, again, other)
	}
}

func TestWindowsSandboxRejectsBroadWritableRoots(t *testing.T) {
	home := sandboxTestHome(t)
	_, sid, err := sandboxSID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{`C:\`, `\\server\share`, `\\server\share\`, home} {
		if err := ensureWritableRoot(root, sid); err == nil || !strings.Contains(err.Error(), "refusing to make") {
			t.Errorf("broad writable root %q: %v", root, err)
		}
	}
}

func TestWindowsSandboxRejectsNetworkOff(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "off")
	sandboxTestHome(t)
	workspace := t.TempDir()
	if _, err := buildCommandWithEnvironment(t.Context(), "exit 0", workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace}); err == nil || !strings.Contains(err.Error(), "not supported on Windows") {
		t.Fatalf("network-off sandbox error = %v", err)
	}
}

func TestWindowsSandboxEnforcesPolicy(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "")
	t.Setenv(workspaceSandboxActiveEnv, "")
	home := sandboxTestHome(t)
	workspace := canonicalPath(t.TempDir())
	outside := canonicalPath(t.TempDir())

	for path, content := range map[string]string{
		filepath.Join(outside, "readable.txt"):         "readable",
		filepath.Join(home, ".ssh", "id_ed25519"):      "secret",
		filepath.Join(workspace, ".git", "config"):     "[core]",
		filepath.Join(workspace, ".git", "hooks", "x"): "hook",
		filepath.Join(workspace, ".vscode", "a.json"):  "{}",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run := func(command string) (string, bool) {
		t.Helper()
		cmd, err := buildCommandWithEnvironment(t.Context(), command, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err == nil
	}
	allowed := map[string]string{
		"workspace write":     `Set-Content inside.txt ok; New-Item -ItemType Directory sub | Out-Null; Set-Content sub\nested.txt ok`,
		"temp write":          `Set-Content (Join-Path $env:TEMP t.txt) ok`,
		"read outside":        `Get-Content '` + filepath.Join(outside, "readable.txt") + `'`,
		"NUL":                 `cmd /c "echo hi> NUL"; if ($LASTEXITCODE) { exit 1 }`,
		"delete own file":     `Set-Content gone.txt x; Remove-Item gone.txt`,
		"git config readable": `Get-Content .git\config`,
		"private temp env":    `if ($env:TEMP -ne $env:TMP -or $env:TEMP -notlike '*\wingman-sandbox\*' -or $env:WINGMAN_SANDBOX_ACTIVE -ne '1') { exit 1 }`,
	}
	for name, command := range allowed {
		if out, ok := run(command); !ok {
			t.Errorf("%s was blocked: %s", name, out)
		}
	}
	denied := map[string]string{
		"write outside":      `Set-Content '` + filepath.Join(outside, "escape.txt") + `' x`,
		"write home":         `Set-Content '` + filepath.Join(home, "escape.txt") + `' x`,
		"read credentials":   `Get-Content '` + filepath.Join(home, ".ssh", "id_ed25519") + `'`,
		"edit git config":    `Add-Content .git\config x`,
		"replace git config": `Set-Content .git\config.new x; Move-Item -Force .git\config.new .git\config`,
		"add git hook":       `Set-Content .git\hooks\pre-commit x`,
		"edit vscode":        `Set-Content .vscode\a.json x`,
		"rename git":         `Rename-Item .git moved`,
		"rename vscode":      `Rename-Item .vscode moved-vscode`,
		"clear root ACL":     `$acl = Get-Acl .; $acl.SetAccessRuleProtection($true, $false); $acl.Access | ForEach-Object { [void]$acl.RemoveAccessRuleSpecific($_) }; $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new('Everyone', 'FullControl', 'ContainerInherit, ObjectInherit', 'None', 'Allow')); Set-Acl . $acl; Add-Content .git\config x`,
		"remove config deny": `$acl = Get-Acl .git\config; $acl.SetAccessRuleProtection($true, $false); $acl.Access | ForEach-Object { [void]$acl.RemoveAccessRuleSpecific($_) }; $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new('Everyone', 'FullControl', 'Allow')); Set-Acl .git\config $acl; Add-Content .git\config x`,
	}
	for name, command := range denied {
		if out, ok := run(command + `; if (-not $?) { exit 1 }`); ok {
			t.Errorf("%s was allowed: %s", name, out)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "sub", "nested.txt")); err != nil {
		t.Errorf("nested write did not land: %v", err)
	}
	for _, path := range []string{filepath.Join(outside, "escape.txt"), filepath.Join(home, "escape.txt")} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s was written", path)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, ".git", "config")); string(data) != "[core]" {
		t.Errorf(".git/config changed: %q", data)
	}
}

func TestWindowsSandboxDoesNotGrantOtherWorkspace(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "")
	sandboxTestHome(t)
	workspace := t.TempDir()
	outside := t.TempDir()
	_, otherSID, err := sandboxSID(outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWritableRoot(outside, otherSID); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, "escape.txt")
	cmd, err := buildCommandWithEnvironment(t.Context(), touchCommand(marker), workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("other workspace was writable: %s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("sandbox escaped to another workspace")
	}
}

func TestWindowsSandboxCancellationKillsBackgroundChildren(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "")
	sandboxTestHome(t)
	workspace := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := `$p = [Diagnostics.Process]::Start((Get-Process -Id $PID).Path, '-NoProfile -Command "Start-Sleep 3; Set-Content late.txt x"'); Set-Content started.txt $p.Id; Start-Sleep 30`
	cmd, err := buildCommandWithEnvironment(ctx, command, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(workspace, "started.txt")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("sandbox did not start its child")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled command succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled command did not exit")
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(filepath.Join(workspace, "late.txt")); err == nil {
		t.Fatal("background child outlived cancellation")
	}
}

func TestWindowsSandboxProtectsWorktreeGitFile(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "")
	sandboxTestHome(t)
	workspace := t.TempDir()
	gitFile := filepath.Join(workspace, ".git")
	content := "gitdir: ../main/.git/worktrees/feature\n"
	if err := os.WriteFile(gitFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, err := buildCommandWithEnvironment(t.Context(), `Set-Content .git 'gitdir: malicious'; if (-not $?) { exit 1 }`, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("worktree .git file was writable: %s", out)
	}
	if data, err := os.ReadFile(gitFile); err != nil || string(data) != content {
		t.Errorf("worktree .git changed: %q, %v", data, err)
	}
}

func TestWindowsSandboxKillsBackgroundChildren(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "")
	t.Setenv(workspaceSandboxActiveEnv, "")
	sandboxTestHome(t)
	workspace := canonicalPath(t.TempDir())
	marker := filepath.Join(workspace, "late.txt")

	command := `$p = [Diagnostics.Process]::Start((Get-Process -Id $PID).Path, '-NoProfile -Command "Start-Sleep 3; Set-Content late.txt x"'); "started $($p.Id)"`
	cmd, err := buildCommandWithEnvironment(t.Context(), command, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("command failed: %v %s", err, out)
	}
	time.Sleep(5 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background child outlived the sandboxed command")
	}
}
