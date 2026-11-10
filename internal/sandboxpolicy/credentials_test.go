package sandboxpolicy

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

// credentialHome creates a home directory with an SSH store, a netrc file and
// Wingman's MCP credentials, and returns their resolved paths.
func credentialHome(t *testing.T) (home, ssh, netrc, mcp string) {
	t.Helper()
	home = Canonical(testenv.UserHome(t))
	wingman := Canonical(testenv.WingmanHome(t))
	ssh, netrc, mcp = filepath.Join(home, ".ssh"), filepath.Join(home, ".netrc"), filepath.Join(wingman, "mcp-credentials.json")
	if err := os.Mkdir(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{filepath.Join(ssh, "id_ed25519"), netrc, mcp} {
		if err := os.WriteFile(file, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home, ssh, netrc, mcp
}

func TestCredentialPathsListsExistingStores(t *testing.T) {
	_, ssh, netrc, mcp := credentialHome(t)
	got := CredentialPaths()
	if want := []string{ssh, netrc, mcp}; !slices.Equal(got, want) {
		t.Fatalf("CredentialPaths() = %v, want %v", got, want)
	}
}

func TestCheckReadResolvesLinksIntoCredentialStores(t *testing.T) {
	home, ssh, netrc, mcp := credentialHome(t)
	workspace := filepath.Join(home, "project")
	if err := os.MkdirAll(filepath.Join(workspace, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirLink, fileLink, nested := filepath.Join(workspace, "linked"), filepath.Join(workspace, "netrc.md"), filepath.Join(workspace, "skills", "keys")
	for link, target := range map[string]string{dirLink: ssh, fileLink: netrc, nested: dirLink} {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	for name, path := range map[string]string{
		"store":              ssh,
		"file in store":      filepath.Join(ssh, "id_ed25519"),
		"file store":         netrc,
		"wingman store":      mcp,
		"missing in store":   filepath.Join(ssh, "new", "key"),
		"directory link":     dirLink,
		"through link":       filepath.Join(dirLink, "id_ed25519"),
		"file link":          fileLink,
		"chained link":       filepath.Join(nested, "id_ed25519"),
		"missing under link": filepath.Join(nested, "new"),
	} {
		err := CheckRead(workspace, path)
		denied, ok := errors.AsType[*CredentialError](err)
		if !ok || denied.Path != path || !strings.Contains(err.Error(), "protected credential location") {
			t.Errorf("%s: CheckRead(%q) = %v", name, path, err)
		}
	}
	for name, path := range map[string]string{
		"workspace":      workspace,
		"workspace file": filepath.Join(workspace, "main.go"),
		"home":           home,
		"sibling name":   filepath.Join(home, ".sshx", "id"),
	} {
		if err := CheckRead(workspace, path); err != nil {
			t.Errorf("%s: CheckRead(%q) = %v", name, path, err)
		}
	}
}

func TestCheckReadMatchesStoresByIdentity(t *testing.T) {
	home, ssh, netrc, _ := credentialHome(t)
	hardlink := filepath.Join(home, "notes.txt")
	if err := os.Link(netrc, hardlink); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := CheckRead("", hardlink); err == nil {
		t.Error("a hard link to a credential file was readable")
	}
	// A case-insensitive volume reaches the store through another spelling.
	upper := filepath.Join(home, ".SSH")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("volume is case-sensitive")
	}
	if err := CheckRead("", filepath.Join(upper, "id_ed25519")); err == nil {
		t.Errorf("CheckRead accepted another spelling of %s", ssh)
	}
}

// A workspace inside a store keeps its own files; the rest of the store, and
// any other store, stays hidden.
func TestCheckReadKeepsWorkspaceInsideStoreUsable(t *testing.T) {
	home, ssh, netrc, _ := credentialHome(t)
	workspace := filepath.Join(ssh, "project")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	guard := NewCredentialGuard(workspace)
	for _, path := range []string{workspace, filepath.Join(workspace, "main.go"), filepath.Join(workspace, "sub", "new.go")} {
		if err := guard.Check(path); err != nil {
			t.Errorf("workspace path %q refused: %v", path, err)
		}
	}
	for _, path := range []string{ssh, filepath.Join(ssh, "id_ed25519"), netrc} {
		if err := guard.Check(path); err == nil {
			t.Errorf("path %q outside the workspace was accepted", path)
		}
	}

	// A workspace that is the store itself hides nothing of its own.
	own := NewCredentialGuard(ssh)
	if err := own.Check(filepath.Join(ssh, "id_ed25519")); err != nil {
		t.Errorf("store used as workspace refused its own file: %v", err)
	}
	if err := own.Check(netrc); err == nil {
		t.Error("another store was accepted")
	}

	// A store inside the workspace stays hidden.
	if err := CheckRead(home, filepath.Join(ssh, "id_ed25519")); err == nil {
		t.Error("store inside the workspace was accepted")
	}
}

// A repository directory that links into a store must not earn the exemption
// when the agent is started inside it.
func TestCheckReadRefusesWorkspaceRedirectedIntoStore(t *testing.T) {
	home, ssh, _, _ := credentialHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(filepath.Join(ssh, "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{filepath.Join(repo, "docs"): ssh, filepath.Join(repo, "nested"): filepath.Join(ssh, "project")} {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		for _, path := range []string{link, filepath.Join(link, "id_ed25519"), filepath.Join(target, "id_ed25519")} {
			if err := CheckRead(link, path); err == nil {
				t.Errorf("workspace %q linked to %q may read %q", link, target, path)
			}
		}
	}
}

func TestCredentialGuardHidesStoreEntries(t *testing.T) {
	home, ssh, netrc, _ := credentialHome(t)
	if err := os.Mkdir(filepath.Join(home, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "sub", ".ssh")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	guard := NewCredentialGuard(home)
	stat := func(path string) func() (os.FileInfo, error) {
		return func() (os.FileInfo, error) { return os.Lstat(path) }
	}
	for _, path := range []string{ssh, netrc} {
		if !guard.Named(filepath.Base(path)) || !guard.Hides(path, stat(path)) {
			t.Errorf("store %q is not hidden", path)
		}
	}
	// Only the store itself is hidden, not every entry sharing its name.
	if !guard.Named(".ssh") || guard.Hides(other, stat(other)) {
		t.Errorf("unrelated directory %q is hidden", other)
	}
	if guard.Named("main.go") {
		t.Error("ordinary name treated as a store candidate")
	}
}
