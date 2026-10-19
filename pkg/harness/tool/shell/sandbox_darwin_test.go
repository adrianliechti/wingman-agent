//go:build darwin

package shell

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSeatbeltProfileUsesParameterizedWritableRoots(t *testing.T) {
	profile, definitions := seatbeltProfile(sandboxPolicy{WritableRoots: []string{"/repo with spaces", "/private/tmp"}, Network: true})

	writeGrant, _, _ := strings.Cut(strings.Split(profile, "(allow file-write*")[1], "(deny appleevent-send)")
	if strings.Contains(writeGrant, "/repo with spaces") {
		t.Fatalf("write grant interpolates an untrusted path directly: %s", writeGrant)
	}
	for _, want := range []string{
		`(deny default)`,
		`(allow file-read*)`,
		`(param "WRITABLE_ROOT_0")`,
		`(deny appleevent-send)`,
		`(deny lsopen)`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing %q", want)
		}
	}
	if !slices.Contains(definitions, "-DWRITABLE_ROOT_0=/repo with spaces") {
		t.Fatalf("definitions = %q", definitions)
	}
	for _, broadGrant := range []string{
		`(allow process*)`,
		`(allow mach*)`,
		`(allow ipc*)`,
		`(allow file-ioctl)`,
		`(allow iokit-open)`,
		`(allow system-socket)`,
	} {
		if strings.Contains(profile, broadGrant) {
			t.Errorf("profile contains broad grant %q", broadGrant)
		}
	}
}

func TestSeatbeltProfileCompiles(t *testing.T) {
	profile, definitions := seatbeltProfile(sandboxPolicy{
		WritableRoots: []string{t.TempDir(), "/private/tmp"},
		ProtectedRoot: filepath.Join(t.TempDir(), `we"ird\ (root)`),
		DenyRead:      []string{t.TempDir()},
		Network:       true,
	})
	args := []string{"-p", profile}
	args = append(args, definitions...)
	args = append(args, "--", "/usr/bin/true")

	output, err := exec.Command(sandboxExecPath, args...).CombinedOutput()
	if err == nil {
		return
	}
	// Some test runners already run under Seatbelt, where applying a second
	// profile is prohibited. sandbox-exec parses the profile before reaching
	// that error, so this still catches malformed policies there.
	if strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
		t.Skip("nested Seatbelt profiles are unavailable")
	}
	t.Fatalf("profile did not compile: %v\n%s", err, output)
}

func TestSeatbeltProfileEnforcesWorkspaceWrites(t *testing.T) {
	workspace := canonicalPath(t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, fmt.Sprintf(".wingman-sandbox-probe-%d-%d", os.Getpid(), time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(outside) })
	inside := filepath.Join(workspace, "inside")

	profile, definitions := seatbeltProfile(sandboxPolicy{WritableRoots: []string{workspace}})
	args := []string{"-p", profile}
	args = append(args, definitions...)
	command := "touch " + shellTestQuote(inside) + " && ! touch " + shellTestQuote(outside)
	args = append(args, "--", "/bin/sh", "-c", command)

	cmd := exec.Command(sandboxExecPath, args...)
	cmd.Dir = workspace
	output, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
		t.Skip("nested Seatbelt profiles are unavailable")
	}
	if err != nil {
		t.Fatalf("sandboxed command failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("workspace write was denied: %v", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("write outside workspace was not denied: %v", err)
	}
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func TestSeatbeltProfileProtectsConfigAndCredentials(t *testing.T) {
	workspace := canonicalPath(t.TempDir())
	secrets := canonicalPath(t.TempDir())
	for _, dir := range []string{".git/hooks", "sub/.git/hooks"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{filepath.Join(workspace, ".git/config"): "[core]\n", filepath.Join(secrets, "key"): "secret"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(workspace, "agent-config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("agent-config", filepath.Join(workspace, ".agents")); err != nil {
		t.Fatal(err)
	}

	profile, definitions := seatbeltProfile(sandboxPolicy{WritableRoots: []string{workspace}, ProtectedRoot: workspace, DenyRead: []string{secrets}})
	checks := []string{
		"touch ok.txt", "! touch .git/ORIG_HEAD",
		"! touch .git/hooks/pre-commit", "! touch sub/.git/hooks/pre-commit", "! sh -c 'echo x >> .git/config'",
		"! mkdir .vscode", "! touch mcp.json", "! mv .git moved", "! cat " + shellTestQuote(filepath.Join(secrets, "key")),
		"! touch agent-config/bypass",
		"! mkdir .CoDeX", "! touch .EnVrC", "! touch .mCp.JsOn",
	}
	args := append([]string{"-p", profile}, definitions...)
	// An && list fails on the first check that misbehaves; set -e would ignore negated commands.
	args = append(args, "--", "/bin/sh", "-c", "{ "+strings.Join(checks, " && ")+"; } 2>/dev/null")
	cmd := exec.Command(sandboxExecPath, args...)
	cmd.Dir = workspace
	output, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
		t.Skip("nested Seatbelt profiles are unavailable")
	}
	if err != nil {
		t.Fatalf("a protection check failed: %v\n%s", err, output)
	}
}

func TestScopedPermissionsRetainSeatbeltBoundaries(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	workspace, extra, outside := t.TempDir(), t.TempDir(), t.TempDir()
	curl := "/usr/bin/curl --noproxy '*' -fsS --max-time 2 " + shellTestQuote(server.URL)
	for _, tc := range []struct {
		name          string
		network, root bool
	}{{"default", false, false}, {"network", true, false}, {"directory", false, true}, {"both", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{}
			if tc.network || tc.root {
				args["sandbox_permissions"] = "require_permissions"
				args["justification"] = "integration fixture"
				args["network"] = tc.network
				if tc.root {
					args["writable_roots"] = []any{extra}
				}
			}
			opts, _, err := executionOptions(workspace, args, &Options{TempDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			networkCheck := curl
			if !tc.network {
				networkCheck = "! " + networkCheck
			}
			writeCheck := "touch " + shellTestQuote(filepath.Join(extra, tc.name))
			if !tc.root {
				writeCheck = "! " + writeCheck
			}
			checks := []string{networkCheck, writeCheck, "! touch " + shellTestQuote(filepath.Join(outside, tc.name)), `touch "$TMPDIR/private"`, `mkdir -p "$TMPDIR/.git"`}
			cmd, err := buildToolCommand(t.Context(), strings.Join(checks, " && "), workspace, workspace, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.CombinedOutput()
			if err != nil && strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
				t.Skip("nested Seatbelt profiles are unavailable")
			}
			if err != nil {
				t.Fatalf("scoped grant changed other boundaries: %v\n%s", err, output)
			}
		})
	}
}

func TestSeatbeltProfileAllowsProtectedNamesInsideTempDir(t *testing.T) {
	workspace := "/repo"
	tempDir := "/private/tmp/wingman-command-123"
	profile, _ := seatbeltProfile(sandboxPolicy{
		WritableRoots: []string{workspace, tempDir},
		TempDir:       tempDir,
		ProtectedRoot: workspace,
	})
	if !strings.Contains(profile, regexp.QuoteMeta(workspace)+"/") {
		t.Fatalf("expected protected rules for workspace in profile:\n%s", profile)
	}
	if strings.Contains(profile, regexp.QuoteMeta(tempDir)+"/") {
		t.Fatalf("tempDir must not have protected subpath deny rules:\n%s", profile)
	}
}

func TestSeatbeltProfileBlocksHostUnixSocketsEvenInsideWritableRoots(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Socket paths are limited to about 104 bytes, too short for t.TempDir.
	short, err := os.MkdirTemp("/tmp", "wm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	workspace := canonicalPath(short)
	inside := filepath.Join(workspace, "s")
	outside := filepath.Join(home, fmt.Sprintf(".wm-sock-%d", os.Getpid()))
	for _, path := range []string{inside, outside} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Skipf("unix sockets unavailable: %v", err)
		}
		t.Cleanup(func() { listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
	}

	profile, definitions := seatbeltProfile(sandboxPolicy{WritableRoots: []string{workspace}, Network: true})
	args := append([]string{"-p", profile}, definitions...)
	args = append(args, "--", "/bin/sh", "-c", "{ ! /usr/bin/nc -U "+shellTestQuote(inside)+" </dev/null && ! /usr/bin/nc -U "+shellTestQuote(outside)+" </dev/null; } 2>/dev/null")
	output, err := exec.Command(sandboxExecPath, args...).CombinedOutput()
	if err != nil && strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
		t.Skip("nested Seatbelt profiles are unavailable")
	}
	if err != nil {
		t.Fatalf("socket reachability is wrong: %v\n%s", err, output)
	}
}

// Each of these services would act outside the sandbox on the command's behalf.
func TestSeatbeltBlocksHostServiceEscapes(t *testing.T) {
	f := newEscapeFixture(t)
	label := fmt.Sprintf("com.wingman.sandbox-test.%d", os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("launchctl", "remove", label).Run()
		_ = exec.Command("defaults", "delete", label).Run()
	})
	runSandboxed(t, f.workspace, "launchctl submit -l "+label+" -- /usr/bin/true; defaults write "+label+" key value", nil)
	if exec.Command("launchctl", "list", label).Run() == nil {
		t.Error("sandboxed command submitted a launchd job")
	}
	if exec.Command("defaults", "read", label).Run() == nil {
		t.Error("sandboxed command wrote user defaults through cfprefsd")
	}
	if exec.Command("pbpaste").Run() == nil {
		if output, _ := runSandboxed(t, f.workspace, "pbpaste >/dev/null && echo CLIPBOARD_READ", nil); strings.Contains(output, "CLIPBOARD_READ") {
			t.Error("sandboxed command read the clipboard")
		}
	}
}
