package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	. "github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

const credentialDenial = "protected credential location"

// credentialStore gives the test a home directory holding an SSH store.
func credentialStore(t *testing.T) (home, store string) {
	t.Helper()
	home = testenv.UserHome(t)
	testenv.WingmanHome(t)
	store = filepath.Join(home, ".ssh")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"id_ed25519": "PRIVATE KEY fixture\n", "config": "Host fixture\n"} {
		if err := os.WriteFile(filepath.Join(store, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home, store
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

type credentialCall struct {
	name string
	tool tool.Tool
	args map[string]any
}

// expectCredentialDenial requires every call to fail naming the credential
// location, without disclosing the stored secret.
func expectCredentialDenial(t *testing.T, calls []credentialCall) {
	t.Helper()
	for _, call := range calls {
		result, err := call.tool.Execute(context.Background(), call.args)
		if err == nil || !strings.Contains(err.Error(), credentialDenial) {
			t.Errorf("%s: got %q, %v; want a %s error", call.name, result.Content.Text(), err, credentialDenial)
		}
		if strings.Contains(result.Content.Text(), "fixture") || (err != nil && strings.Contains(err.Error(), "fixture")) {
			t.Errorf("%s disclosed credential content: %q, %v", call.name, result.Content.Text(), err)
		}
	}
}

// A repository can ship its skills root as a link. One that leads to shared
// skills keeps working; one that leads to a credential store is refused, also
// when the link appears after the tools were created.
func TestLinkedSkillRootIntoCredentialStoreIsRefused(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			_, store := credentialStore(t)
			workspace, shared := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(shared, "review"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(shared, "review", "SKILL.md"), []byte("---\nname: review\ndescription: Review code.\n---\nShared review guidance.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			ordinary, hostile := filepath.Join(workspace, ".agents", "skills"), filepath.Join(workspace, ".claude", "skills")
			for _, link := range []string{ordinary, hostile} {
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			root := openRoot(t, workspace)
			allowed := []string{ordinary, hostile}
			read, glob, grep := ReadTool(root, allowed...), GlobTool(root, allowed...), GrepTool(root, allowed...)
			// The links are created after the session captured its read roots.
			testenv.DirLink(t, kind, shared, ordinary)
			testenv.DirLink(t, kind, store, hostile)

			for _, path := range []string{hostile, filepath.Join(".claude", "skills")} {
				expectCredentialDenial(t, []credentialCall{
					{"read " + path, read, map[string]any{"file_path": filepath.Join(path, "id_ed25519")}},
					{"grep " + path, grep, map[string]any{"path": path, "pattern": "fixture", "output_mode": "content"}},
					{"grep file " + path, grep, map[string]any{"path": filepath.Join(path, "config"), "pattern": "Host"}},
					{"glob " + path, glob, map[string]any{"path": path, "pattern": "**/*"}},
					{"glob pattern " + path, glob, map[string]any{"pattern": filepath.Join(workspace, ".claude", "skills", "*")}},
				})
			}
			for _, check := range []struct {
				name string
				tool tool.Tool
				args map[string]any
				want string
			}{
				{"read", read, map[string]any{"file_path": filepath.Join(ordinary, "review", "SKILL.md")}, "name: review"},
				{"glob", glob, map[string]any{"path": ordinary, "pattern": "**/*.md"}, "SKILL.md"},
				{"grep", grep, map[string]any{"path": filepath.Join(".agents", "skills"), "pattern": "Shared review", "output_mode": "content"}, "Shared review guidance"},
			} {
				result, err := check.tool.Execute(context.Background(), check.args)
				if err != nil || !strings.Contains(result.Content.Text(), check.want) {
					t.Errorf("%s through the ordinary linked root = %q, %v", check.name, result.Content.Text(), err)
				}
			}
		})
	}
}

// A link nested inside an allowed root is refused as a credential location,
// both when only the enclosing root is granted and when the linked skill
// directory holds its own grant.
func TestNestedLinkIntoCredentialStoreIsRefused(t *testing.T) {
	for _, granted := range []bool{false, true} {
		_, store := credentialStore(t)
		workspace := t.TempDir()
		skills := filepath.Join(workspace, ".agents", "skills")
		if err := os.MkdirAll(skills, 0o755); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(skills, "keys")
		testenv.DirLink(t, "symlink", store, nested)
		if err := os.Symlink(filepath.Join(store, "config"), filepath.Join(skills, "notes.md")); err != nil {
			t.Fatal(err)
		}
		root := openRoot(t, workspace)
		allowed := []string{skills}
		if granted {
			allowed = append(allowed, nested)
		}
		read, glob, grep := ReadTool(root, allowed...), GlobTool(root, allowed...), GrepTool(root, allowed...)
		expectCredentialDenial(t, []credentialCall{
			{"read", read, map[string]any{"file_path": filepath.Join(nested, "id_ed25519")}},
			{"read relative", read, map[string]any{"file_path": filepath.Join(".agents", "skills", "keys", "id_ed25519")}},
			{"read file link", read, map[string]any{"file_path": filepath.Join(skills, "notes.md")}},
			{"grep", grep, map[string]any{"path": nested, "pattern": "fixture"}},
			{"glob", glob, map[string]any{"path": nested, "pattern": "*"}},
		})
		// Searching the enclosing root never follows the links.
		for _, search := range []credentialCall{
			{"glob", glob, map[string]any{"path": skills, "pattern": "**/*"}},
			{"grep", grep, map[string]any{"path": skills, "pattern": "fixture|Host", "output_mode": "content"}},
		} {
			result, err := search.tool.Execute(context.Background(), search.args)
			if err != nil || strings.Contains(result.Content.Text(), "id_ed25519") || strings.Contains(result.Content.Text(), "fixture") {
				t.Errorf("granted=%t: %s of the enclosing root = %q, %v", granted, search.name, result.Content.Text(), err)
			}
		}
	}
}

func TestEditIntoCredentialStoreIsRefused(t *testing.T) {
	home, store := credentialStore(t)
	workspace := t.TempDir()
	memory := filepath.Join(workspace, "memory")
	testenv.DirLink(t, "symlink", store, memory)
	root := openRoot(t, workspace)
	if err := os.WriteFile(filepath.Join(workspace, "local.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, edit := range map[string]tool.Tool{"linked write root": EditTool(root, memory), "any path": EditTool(root, "*")} {
		expectCredentialDenial(t, []credentialCall{
			{name + " create", edit, createArgs(filepath.Join(memory, "authorized_keys"), "ssh-ed25519 attacker")},
			{name + " replace", edit, map[string]any{"edits": []any{
				map[string]any{"file_path": "local.txt", "old_string": "original", "new_string": "changed"},
				map[string]any{"file_path": filepath.Join(memory, "config"), "old_string": "Host fixture", "new_string": "Host attacker"},
			}}},
		})
	}
	expectCredentialDenial(t, []credentialCall{
		{"wildcard read", ReadTool(root, "*"), map[string]any{"file_path": filepath.Join(home, ".ssh", "id_ed25519")}},
		{"wildcard home read", ReadTool(root, "*"), map[string]any{"file_path": "~/.ssh/id_ed25519"}},
	})
	if _, err := os.Stat(filepath.Join(store, "authorized_keys")); !os.IsNotExist(err) {
		t.Errorf("edit created a file in the credential store: %v", err)
	}
	for file, want := range map[string]string{filepath.Join(store, "config"): "Host fixture\n", filepath.Join(workspace, "local.txt"): "original"} {
		if data, err := os.ReadFile(file); err != nil || string(data) != want {
			t.Errorf("refused edit changed %s: %q, %v", file, data, err)
		}
	}
}

// A workspace that contains a store, such as a home directory, still works;
// the store is skipped by searches and refused when addressed.
func TestCredentialStoreInsideWorkspaceIsHidden(t *testing.T) {
	home, _ := credentialStore(t)
	if err := os.MkdirAll(filepath.Join(home, "project", ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"notes.txt":                             "fixture notes\n",
		filepath.Join("project", "main.go"):     "package fixture\n",
		filepath.Join("project", ".ssh", "doc"): "fixture doc\n",
	} {
		if err := os.WriteFile(filepath.Join(home, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := openRoot(t, home)
	read, glob, grep, edit := ReadTool(root), GlobTool(root), GrepTool(root), EditTool(root)

	for _, call := range []credentialCall{
		{"read", read, map[string]any{"file_path": filepath.Join(".ssh", "id_ed25519")}},
		{"grep", grep, map[string]any{"path": ".ssh", "pattern": "fixture"}},
		{"glob", glob, map[string]any{"path": ".ssh", "pattern": "*"}},
		{"glob pattern", glob, map[string]any{"pattern": ".ssh/*"}},
		{"edit", edit, createArgs(filepath.Join(".ssh", "authorized_keys"), "ssh-ed25519 attacker")},
	} {
		if _, err := call.tool.Execute(context.Background(), call.args); err == nil || !strings.Contains(err.Error(), credentialDenial) {
			t.Errorf("%s of a store inside the workspace: %v", call.name, err)
		}
	}

	// A relative link stays inside the workspace handle, so only the
	// credential check stands between it and the store.
	if err := os.Symlink(".ssh", filepath.Join(home, "alias")); err == nil {
		expectCredentialDenial(t, []credentialCall{
			{"read through link", read, map[string]any{"file_path": filepath.Join("alias", "id_ed25519")}},
			{"grep through link", grep, map[string]any{"path": "alias", "pattern": "fixture"}},
			{"glob through link", glob, map[string]any{"path": "alias", "pattern": "*"}},
			{"edit through link", edit, createArgs(filepath.Join("alias", "authorized_keys"), "ssh-ed25519 attacker")},
		})
		if _, err := os.Stat(filepath.Join(home, ".ssh", "authorized_keys")); !os.IsNotExist(err) {
			t.Errorf("edit through a link created a file in the credential store: %v", err)
		}
	}

	listed, err := glob.Execute(context.Background(), map[string]any{"pattern": "**/*"})
	if err != nil {
		t.Fatal(err)
	}
	matched, err := grep.Execute(context.Background(), map[string]any{"pattern": "fixture", "output_mode": "content"})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"glob": listed.Content.Text(), "grep": matched.Content.Text()} {
		if strings.Contains(content, "id_ed25519") || strings.Contains(content, "PRIVATE KEY") || strings.Contains(content, "Host fixture") {
			t.Errorf("%s walked into the credential store:\n%s", name, content)
		}
		// Only the store itself is hidden, not every directory sharing its name.
		for _, want := range []string{"notes.txt", filepath.Join("project", "main.go"), filepath.Join("project", ".ssh", "doc")} {
			if !strings.Contains(content, want) {
				t.Errorf("%s lost ordinary file %s:\n%s", name, want, content)
			}
		}
	}
}

// A workspace located inside a store is not locked out of its own files.
func TestWorkspaceInsideCredentialStoreKeepsItsFiles(t *testing.T) {
	_, store := credentialStore(t)
	workspace := filepath.Join(store, "project")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := openRoot(t, workspace)
	for _, check := range []struct {
		tool tool.Tool
		args map[string]any
		want string
	}{
		{ReadTool(root), map[string]any{"file_path": "main.go"}, "package project"},
		{GlobTool(root), map[string]any{"pattern": "*.go"}, "main.go"},
		{GrepTool(root), map[string]any{"pattern": "package", "output_mode": "content"}, "package project"},
		{EditTool(root), createArgs("new.go", "package project\n"), "new.go"},
	} {
		result, err := check.tool.Execute(context.Background(), check.args)
		if err != nil || !strings.Contains(result.Content.Text(), check.want) {
			t.Errorf("%s in a workspace inside a store = %q, %v", check.tool.Name, result.Content.Text(), err)
		}
	}
	// The rest of the store stays out of reach, even through an explicit grant.
	expectCredentialDenial(t, []credentialCall{
		{"read", ReadTool(root, store), map[string]any{"file_path": filepath.Join(store, "id_ed25519")}},
		{"glob", GlobTool(root, store), map[string]any{"path": store, "pattern": "*"}},
	})
}

// Starting the agent in a repository directory that links into a store does
// not make that store the workspace's own.
func TestWorkspaceRedirectedIntoCredentialStoreIsRefused(t *testing.T) {
	_, store := credentialStore(t)
	docs := filepath.Join(t.TempDir(), "docs")
	testenv.DirLink(t, "symlink", store, docs)
	root := openRoot(t, docs)
	expectCredentialDenial(t, []credentialCall{
		{"read", ReadTool(root), map[string]any{"file_path": "id_ed25519"}},
		{"grep", GrepTool(root), map[string]any{"pattern": "fixture"}},
		{"glob", GlobTool(root), map[string]any{"pattern": "*"}},
		{"edit", EditTool(root), createArgs("authorized_keys", "ssh-ed25519 attacker")},
	})
}
