package shell

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func TestExecutionContextPreservesInheritedSandbox(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "1")
	for _, opts := range []*Options{nil, {DisableSandbox: true}} {
		got := ExecutionContext(t.TempDir(), opts)
		if !strings.Contains(got, "inherited") || strings.Contains(got, "disabled") {
			t.Fatalf("context claims inherited sandbox can be disabled: %s", got)
		}
	}
}

func TestExecutionContextKeepsPrivateTempWithDisabledOrInheritedSandbox(t *testing.T) {
	for _, inherited := range []string{"", "1"} {
		t.Setenv(workspaceSandboxActiveEnv, inherited)
		opts := &Options{TempDir: t.TempDir(), DisableSandbox: true}
		got := ExecutionContext(t.TempDir(), opts)
		if !strings.Contains(got, "TMPDIR = TEMP = TMP = `"+opts.TempDir+"`") {
			t.Fatalf("context omits the command's private temp: %s", got)
		}
		if strings.Contains(got, "Shell writable roots:") || strings.Contains(got, "Shell IP network:") || strings.Contains(got, "Escalated") {
			t.Fatalf("context advertises an inactive local policy: %s", got)
		}
	}
}

// braceList returns the entries of the "{a,b}" group that follows prefix.
func braceList(t *testing.T, text, prefix string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(text, prefix+"{")
	list, _, closed := strings.Cut(rest, "}")
	if !ok || !closed {
		t.Fatalf("no %q list in: %s", prefix, text)
	}
	return strings.Split(list, ",")
}

// The exec description names what the sandbox hides and protects. It must name
// exactly what the backends enforce, including entries added to the policy later.
func TestSandboxGuideNamesTheEnforcedPaths(t *testing.T) {
	home := canonicalPath(testenv.UserHome(t))
	wingman := canonicalPath(testenv.WingmanHome(t))
	workspace := t.TempDir()

	check := func() {
		t.Helper()
		for _, rel := range sandboxpolicy.CredentialHomePaths {
			path := filepath.Join(home, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, nil, 0o600); err != nil && !os.IsExist(err) {
				t.Fatal(err)
			}
		}
		for _, name := range slices.Concat(sandboxpolicy.CredentialStateFiles, protectedDirs, protectedFiles, protectedRootFiles) {
			for _, dir := range []string{wingman, workspace} {
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		guide := sandboxGuideLine(nil)

		// Every path the backends deny reading is named, and nothing else is.
		var named []string
		for _, rel := range braceList(t, guide, "~/") {
			named = append(named, filepath.Join(home, filepath.FromSlash(rel)))
		}
		for _, name := range sandboxpolicy.CredentialStateFiles {
			if !strings.Contains(guide, name) {
				t.Errorf("guide does not name %s", name)
			}
			named = append(named, filepath.Join(wingman, name))
		}
		if denied := credentialPaths(); !slices.Equal(named, denied) {
			t.Errorf("guide names %v\nsandbox denies %v", named, denied)
		}

		// Every path the backends keep read-only at a project root is named.
		named = nil
		for _, name := range slices.Concat(braceList(t, guide, "project roots: "), braceList(t, guide, "root-level ")) {
			named = append(named, filepath.Join(canonicalPath(workspace), name))
		}
		for _, file := range protectedFiles {
			if !strings.Contains(guide, "} and "+file+" at any depth") {
				t.Errorf("guide does not name %s", file)
			}
			named = append(named, filepath.Join(canonicalPath(workspace), file))
		}
		protected := existingProtectedPaths(canonicalPath(workspace))
		slices.Sort(named)
		slices.Sort(protected)
		if !slices.Equal(named, protected) {
			t.Errorf("guide names %v\nsandbox protects %v", named, protected)
		}
	}
	check()

	// Extending a policy list extends the description with it.
	restore := func(list *[]string) func() {
		original := slices.Clone(*list)
		return func() { *list = original }
	}
	defer restore(&sandboxpolicy.CredentialHomePaths)()
	defer restore(&sandboxpolicy.CredentialStateFiles)()
	defer restore(&protectedDirs)()
	defer restore(&protectedRootFiles)()
	sandboxpolicy.CredentialHomePaths = append(sandboxpolicy.CredentialHomePaths, ".fixture-store")
	sandboxpolicy.CredentialStateFiles = append(sandboxpolicy.CredentialStateFiles, "fixture-credentials.json")
	protectedDirs = append(protectedDirs, ".fixture-editor")
	protectedRootFiles = append(protectedRootFiles, ".fixturerc")
	check()
}

// The denial notice must name a next step the exec schema accepts.
func TestSandboxDenialNoticeNamesTheNextStep(t *testing.T) {
	// The schema only offers sandbox permissions when this runner owns the sandbox.
	t.Setenv(workspaceSandboxActiveEnv, "")
	manager := NewExecManager(nil)
	defer manager.Close()
	properties := ExecTools(manager, t.TempDir(), nil, nil)[0].Parameters["properties"].(map[string]any)
	modes := properties["sandbox_permissions"].(map[string]any)["enum"].([]string)
	if !slices.Contains(modes, "require_permissions") || properties["network"] == nil || properties["writable_roots"] == nil {
		t.Fatalf("exec schema lacks scoped permissions: %v", properties)
	}
	for _, want := range []string{`sandbox_permissions: "require_permissions"`, "network: true", "writable_roots", "host credentials keep default permissions"} {
		if !strings.Contains(sandboxDenialNotice, want) {
			t.Errorf("denial notice does not name %q: %s", want, sandboxDenialNotice)
		}
	}
	if strings.Contains(strings.ToLower(sandboxDenialNotice), "do not request") {
		t.Errorf("denial notice discourages permission requests: %s", sandboxDenialNotice)
	}
}
