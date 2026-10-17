//go:build windows

package shell

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsSandboxRejectsBroadWritableRoots(t *testing.T) {
	home := sandboxTestHome(t)
	for _, root := range []string{`C:\`, `\\server\share`, `\\server\share\`, home} {
		if err := validateSandboxWritableRoot(root); err == nil || !strings.Contains(err.Error(), "refusing to make") {
			t.Errorf("broad writable root %q: %v", root, err)
		}
	}
}

func TestWindowsSandboxLaunchesWithNetworkOff(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "off")
	sandboxTestHome(t)
	workspace := t.TempDir()
	cmd, err := buildCommandWithEnvironment(t.Context(), "exit 0", workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("network-off sandbox: %v %s", err, out)
	}
}

func TestWindowsSandboxUnsupportedFailsBeforeLaunch(t *testing.T) {
	workspace := t.TempDir()
	before := sandboxSecuritySnapshot(t, workspace)
	original := checkNativeSandboxSupport
	checkNativeSandboxSupport = func() error { return nativeSandboxUnavailable(errors.New("test: no native denied paths")) }
	t.Cleanup(func() { checkNativeSandboxSupport = original })
	_, err := buildCommandWithEnvironment(t.Context(), `Set-Content should-not-run.txt x`, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
	if err == nil || !strings.Contains(err.Error(), "native Windows sandbox is unavailable") {
		t.Fatalf("unsupported sandbox: %v", err)
	}
	assertSandboxSecurityUnchanged(t, before)
}

func TestWindowsSandboxNetworkPolicy(t *testing.T) {
	sandboxTestHome(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local network fixture unavailable: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	command := fmt.Sprintf(`$c = [Net.Sockets.TcpClient]::new(); try { $t = $c.ConnectAsync('127.0.0.1', %d); if (-not $t.Wait(3000)) { throw 'timeout' }; 'connected' } catch { Write-Output $_; exit 1 } finally { $c.Dispose() }`, port)
	workspace := t.TempDir()
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			value := "off"
			if enabled {
				value = "on"
			}
			t.Setenv(workspaceSandboxNetworkEnv, value)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd, err := buildCommandWithEnvironment(ctx, command, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.CombinedOutput()
			if (err == nil) != enabled {
				t.Fatalf("network enabled=%t: %v %s", enabled, err, out)
			}
		})
	}
}

func TestWindowsSandboxLaunchFailureDoesNotChangeSecurity(t *testing.T) {
	sandboxTestHome(t)
	workspace := t.TempDir()
	before := sandboxSecuritySnapshot(t, workspace)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sandboxLaunch{
		Policy: sandboxPolicy{WritableRoots: []string{workspace}, ProtectedRoot: workspace, Network: true},
		Argv:   []string{filepath.Join(workspace, "missing.exe")},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, sandboxHelperArg, base64.RawURLEncoding.EncodeToString(raw))
	cmd.Dir = workspace
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "start ") {
		t.Fatalf("launch failure: %v %s", err, out)
	}
	assertSandboxSecurityUnchanged(t, before)
}

func TestWindowsSandboxSecurityEditBoundaries(t *testing.T) {
	sandboxTestHome(t)
	for _, location := range []string{"workspace", "protected", "outside"} {
		t.Run(location, func(t *testing.T) {
			workspace := t.TempDir()
			target := workspace
			if location == "protected" {
				target = filepath.Join(workspace, ".vscode")
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if location == "outside" {
				target = t.TempDir()
			}
			before := sandboxSecuritySnapshot(t, target)
			command := `$ErrorActionPreference = 'Stop'; $acl = Get-Acl -LiteralPath '` + target + `'; $acl.SetAccessRuleProtection($true, $false); $acl.Access | ForEach-Object { [void]$acl.RemoveAccessRuleSpecific($_) }; $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new('Everyone', 'FullControl', 'ContainerInherit, ObjectInherit', 'None', 'Allow')); Set-Acl -LiteralPath '` + target + `' $acl`
			cmd, err := buildCommandWithEnvironment(t.Context(), command, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.CombinedOutput()
			if location == "workspace" {
				// PSEC grants the normal filesystem write rights, including ACL
				// changes, inside writable roots. This is workload-authored state.
				if err != nil {
					t.Fatalf("writable-root security edit: %v %s", err, out)
				}
			} else {
				if err == nil {
					t.Fatalf("%s security edit was allowed: %s", location, out)
				}
				assertSandboxSecurityUnchanged(t, before)
			}
		})
	}
}

func TestWindowsSandboxRejectsJunctionAndHardlinkEscapes(t *testing.T) {
	sandboxTestHome(t)
	workspace := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.txt")
	credential := filepath.Join(outside, "credential.txt")
	if err := os.WriteFile(credential, []byte("fixture-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "hardlink.txt")
	if err := os.Link(outsideFile, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(credential, filepath.Join(workspace, "credential-link.txt")); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(workspace, "junction")
	setup := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `New-Item -ItemType Junction -Path '`+junction+`' -Target '`+outside+`' | Out-Null`)
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("junction fixture: %v %s", err, out)
	}
	// Unlink the junction itself before TempDir cleanup; never recurse through it.
	t.Cleanup(func() { os.Remove(junction) })
	for name, command := range map[string]string{
		"junction write":  `Set-Content junction\escape.txt x`,
		"hardlink write":  `Set-Content hardlink.txt changed`,
		"hardlink read":   `Get-Content credential-link.txt`,
		"create hardlink": `New-Item -ItemType HardLink -Path newlink.txt -Target '` + outsideFile + `'`,
	} {
		t.Run(name, func(t *testing.T) {
			cmd, err := buildCommandWithEnvironment(t.Context(), `$ErrorActionPreference = 'Stop'; `+command, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
			if err != nil {
				t.Fatal(err)
			}
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Errorf("%s was allowed: %s", name, out)
			}
		})
	}
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "original" {
		t.Errorf("outside hardlink target changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); err == nil {
		t.Error("junction target was written")
	}
}

func TestWindowsSandboxForcedHelperKillReapsChildren(t *testing.T) {
	sandboxTestHome(t)
	workspace := t.TempDir()
	before := sandboxSecuritySnapshot(t, workspace)
	defer assertSandboxSecurityUnchanged(t, before)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := `$p = [Diagnostics.Process]::Start((Get-Process -Id $PID).Path, '-NoProfile -Command "Start-Sleep 30"'); Set-Content started.txt $p.Id; Start-Sleep 30`
	cmd, err := buildCommandWithEnvironment(ctx, command, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	// Kill only the helper to exercise the job, not taskkill's tree traversal.
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var child windows.Handle
	deadline := time.Now().Add(10 * time.Second)
	for child == 0 {
		if data, err := os.ReadFile(filepath.Join(workspace, "started.txt")); err == nil {
			pid, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff")), 10, 32)
			if err == nil {
				child, err = windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		select {
		case err := <-done:
			t.Fatalf("helper exited before child: %v %s", err, output.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("child startup timeout: %s", output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer windows.CloseHandle(child)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Error("killed helper succeeded")
	}
	if status, err := windows.WaitForSingleObject(child, 5000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("child survived helper kill: status=%d error=%v", status, err)
	}
}

func TestWindowsSandboxRejectsLowEveryoneWritableOutside(t *testing.T) {
	sandboxTestHome(t)
	workspace := t.TempDir()
	outside := t.TempDir()
	// Fixture-only security changes model the old restricted-token escape.
	// The new runtime never installs these descriptors.
	sd, err := windows.SecurityDescriptorFromString("D:PAI(A;OICI;FA;;;WD)S:(ML;OICI;NW;;;LW)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	sacl, _, err := sd.SACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, sacl); err != nil {
		t.Fatal(err)
	}
	before := sandboxSecuritySnapshot(t, outside)
	marker := filepath.Join(outside, "escape.txt")
	cmd, err := buildCommandWithEnvironment(t.Context(), `$ErrorActionPreference = 'Stop'; `+touchCommand(marker), workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("Low Everyone-writable outside directory was writable: %s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("sandbox wrote outside its policy")
	}
	assertSandboxSecurityUnchanged(t, before)
}

func sandboxSecuritySnapshot(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, path := range paths {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatalf("read security descriptor %q: %v", path, err)
		}
		result[path] = sd.String()
	}
	return result
}

func assertSandboxSecurityUnchanged(t *testing.T, before map[string]string) {
	t.Helper()
	for path, want := range before {
		if got := sandboxSecuritySnapshot(t, path)[path]; got != want {
			t.Errorf("security descriptor changed for %q:\nwas %s\nnow %s", path, want, got)
		}
	}
}

func TestWindowsSandboxPowerShellStartup(t *testing.T) {
	sandboxTestHome(t)
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		t.Run(name, func(t *testing.T) {
			shell, err := exec.LookPath(name)
			if err != nil {
				t.Skipf("%s is not installed", name)
			}
			workspace := t.TempDir()
			before := sandboxSecuritySnapshot(t, workspace)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd, err := buildSandboxedCommand(ctx, shell, `Write-Output "TEMP=$env:TEMP TMP=$env:TMP ACTIVE=$env:WINGMAN_SANDBOX_ACTIVE"; Set-Content (Join-Path $env:TEMP startup.txt) ok; if (-not $?) { exit 1 }; Set-Content inside.txt ok; if (-not $?) { exit 1 }`, workspace, sandboxOptions{WorkspaceDir: workspace})
			if err != nil {
				t.Fatal(err)
			}
			cmd.Dir = workspace
			out, err := cmd.CombinedOutput()
			t.Logf("%s", out)
			if err != nil {
				t.Fatalf("PowerShell startup: %v", err)
			}
			assertSandboxSecurityUnchanged(t, before)
		})
	}
}

func TestWindowsSandboxEnforcesPolicy(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "")
	t.Setenv(workspaceSandboxActiveEnv, "")
	home := sandboxTestHome(t)
	workspace := canonicalPath(t.TempDir())
	outside := canonicalPath(t.TempDir())
	memory := canonicalPath(t.TempDir())

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
	before := sandboxSecuritySnapshot(t, workspace, memory, outside, home, filepath.Join(home, ".ssh"), filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(workspace, ".git"), filepath.Join(workspace, ".git", "config"), filepath.Join(workspace, ".git", "hooks"), filepath.Join(workspace, ".vscode"))
	defer assertSandboxSecurityUnchanged(t, before)

	run := func(command string) (string, bool) {
		t.Helper()
		cmd, err := buildCommandWithEnvironment(t.Context(), command, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace, ExtraWritableRoots: []string{memory}})
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err == nil
	}
	allowed := map[string]string{
		"workspace write":     `Set-Content inside.txt ok; New-Item -ItemType Directory sub | Out-Null; Set-Content sub\nested.txt ok`,
		"temp write":          `Set-Content (Join-Path $env:TEMP t.txt) ok`,
		"memory write":        `Set-Content '` + filepath.Join(memory, "memory.txt") + `' ok`,
		"read outside":        `Get-Content '` + filepath.Join(outside, "readable.txt") + `'`,
		"NUL":                 `cmd /c "echo hi> NUL"; if ($LASTEXITCODE) { exit 1 }`,
		"delete own file":     `Set-Content gone.txt x; Remove-Item gone.txt`,
		"git config readable": `Get-Content .git\config`,
		"private temp env":    `if ($env:TEMP -ne $env:TMP -or $env:WINGMAN_SANDBOX_ACTIVE -ne '1') { Write-Output "TEMP=$env:TEMP TMP=$env:TMP ACTIVE=$env:WINGMAN_SANDBOX_ACTIVE"; exit 1 }`,
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
		"rename workspace":   `Rename-Item -LiteralPath '` + workspace + `' -NewName moved-workspace`,
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
	before := sandboxSecuritySnapshot(t, workspace)
	defer assertSandboxSecurityUnchanged(t, before)
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

func TestWindowsSandboxReadsGitWorktree(t *testing.T) {
	sandboxTestHome(t)
	git, err := exec.LookPath("git.exe")
	if err != nil {
		t.Skip("Git is not installed")
	}
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), git, args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Git fixture: %v %s", err, out)
		}
	}
	runGit("init")
	runGit("-c", "user.name=Sandbox Test", "-c", "user.email=sandbox@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	workspace := filepath.Join(t.TempDir(), "worktree")
	runGit("worktree", "add", "--detach", workspace, "HEAD")
	before := sandboxSecuritySnapshot(t, workspace, filepath.Join(workspace, ".git"), filepath.Join(repo, ".git"), filepath.Join(repo, ".git", "config"))
	defer assertSandboxSecurityUnchanged(t, before)
	for name, command := range map[string]string{
		"status":       `git status --porcelain; if ($LASTEXITCODE) { exit 1 }`,
		"config read":  `git config --local --get core.repositoryformatversion; if ($LASTEXITCODE) { exit 1 }`,
		"config write": `git config --local wingman.fixture changed; if (-not $LASTEXITCODE) { exit 1 }; exit 0`,
	} {
		t.Run(name, func(t *testing.T) {
			cmd, err := buildCommandWithEnvironment(t.Context(), command, workspace, os.Environ(), &sandboxOptions{WorkspaceDir: workspace})
			if err != nil {
				t.Fatal(err)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("Git %s: %v %s", name, err, out)
			}
		})
	}
}

func TestWindowsSandboxKillsBackgroundChildren(t *testing.T) {
	t.Setenv(workspaceSandboxNetworkEnv, "")
	t.Setenv(workspaceSandboxActiveEnv, "")
	sandboxTestHome(t)
	workspace := canonicalPath(t.TempDir())
	before := sandboxSecuritySnapshot(t, workspace)
	defer assertSandboxSecurityUnchanged(t, before)
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
