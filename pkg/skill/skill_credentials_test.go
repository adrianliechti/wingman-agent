package skill_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	. "github.com/adrianliechti/wingman-agent/pkg/skill"
)

// credentialSkillStore creates a home whose SSH store holds a file that parses
// as a skill, the worst case for a loader that follows links.
func credentialSkillStore(t *testing.T) (home, store string) {
	t.Helper()
	home = testenv.UserHome(t)
	testenv.WingmanHome(t)
	store = filepath.Join(home, ".ssh")
	writeSkill(t, store, "leak", "Credential fixture")
	writeSkill(t, filepath.Join(store, "nested"), "nested-leak", "Credential fixture")
	return home, store
}

// captureStderr returns what fn reports on the skill diagnostics channel.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	defer func() { os.Stderr = original }()
	output := make(chan string)
	go func() {
		data, _ := io.ReadAll(reader)
		output <- string(data)
	}()
	fn()
	writer.Close()
	os.Stderr = original
	return <-output
}

func TestDiscoverSkipsSkillsLinkedIntoCredentialStore(t *testing.T) {
	for _, placement := range []string{"root", "skill", "file"} {
		t.Run(placement, func(t *testing.T) {
			_, store := credentialSkillStore(t)
			workspace := t.TempDir()
			source := filepath.Join(workspace, ".claude", "skills")
			writeSkill(t, filepath.Join(workspace, ".agents", "skills", "review"), "review", "Ordinary skill")
			var link string
			switch placement {
			case "root":
				link = source
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				testenv.DirLink(t, "symlink", store, link)
			case "skill":
				link = filepath.Join(source, "leak")
				if err := os.MkdirAll(source, 0o755); err != nil {
					t.Fatal(err)
				}
				testenv.DirLink(t, "symlink", store, link)
			case "file":
				link = filepath.Join(source, "leak", "SKILL.md")
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(store, "SKILL.md"), link); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}

			var skills []Skill
			warning := captureStderr(t, func() { skills, _ = Discover(workspace) })
			if len(skills) != 1 || skills[0].Name != "review" {
				t.Fatalf("discovered skills = %#v; want the ordinary skill only", skills)
			}
			if !strings.Contains(warning, "skipped "+link) || !strings.Contains(warning, "protected credential location") {
				t.Fatalf("warning = %q; want a skip naming %s as a credential location", warning, link)
			}
			if strings.Contains(warning, "Credential fixture") {
				t.Fatalf("warning disclosed credential content: %q", warning)
			}
			if skills, _ := Rediscover(workspace); len(skills) != 1 {
				t.Fatalf("rediscovered skills = %#v", skills)
			}
		})
	}
}

// A discovered skill must not become a way to read a credential store when its
// file or directory is replaced by a link later in the session.
func TestSkillContentRefusesLinkCreatedAfterDiscovery(t *testing.T) {
	for _, replace := range []string{"file", "directory"} {
		t.Run(replace, func(t *testing.T) {
			_, store := credentialSkillStore(t)
			workspace := t.TempDir()
			dir := filepath.Join(workspace, ".agents", "skills", "review")
			writeSkill(t, dir, "review", "Ordinary skill")
			skills, err := Discover(workspace)
			if err != nil || len(skills) != 1 {
				t.Fatalf("discovered skills = %#v, %v", skills, err)
			}
			if _, err := skills[0].GetContent(workspace); err != nil {
				t.Fatalf("ordinary skill content: %v", err)
			}

			if replace == "file" {
				file := filepath.Join(dir, "SKILL.md")
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(store, "SKILL.md"), file); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				testenv.DirLink(t, "symlink", store, dir)
			}
			content, err := skills[0].GetContent(workspace)
			if err == nil || !strings.Contains(err.Error(), "protected credential location") || content != "" {
				t.Fatalf("content after the %s became a link = %q, %v", replace, content, err)
			}
			if _, err := LoadFile(filepath.Join(dir, "SKILL.md")); err == nil {
				t.Fatal("LoadFile followed a link into a credential store")
			}
		})
	}
}

// Personal skills kept in a project that lives inside a store stay loadable.
func TestDiscoverInsideCredentialStoreWorkspace(t *testing.T) {
	_, store := credentialSkillStore(t)
	workspace := filepath.Join(store, "project")
	writeSkill(t, filepath.Join(workspace, ".agents", "skills", "review"), "review", "Ordinary skill")
	skills, err := Discover(workspace)
	if err != nil || len(skills) != 1 || skills[0].Name != "review" {
		t.Fatalf("discovered skills = %#v, %v", skills, err)
	}
	if content, err := skills[0].GetContent(workspace); err != nil || !strings.Contains(content, "# review") {
		t.Fatalf("content = %q, %v", content, err)
	}
}
