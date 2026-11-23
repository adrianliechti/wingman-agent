package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
)

func installPlugin(t *testing.T, dir, name string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	manifest := `{"$schema": "` + ManifestSchema + `", "name": "` + name + `"}`

	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0644); err != nil {
		t.Fatalf("write manifest %s: %v", dir, err)
	}
}

func isolateHome(t *testing.T) (string, string) {
	t.Helper()

	return testenv.UserHome(t), testenv.WingmanHome(t)
}

func TestDiscoverPrefersWingmanOverAgentsAndClaude(t *testing.T) {
	isolateHome(t)

	work := t.TempDir()

	installPlugin(t, filepath.Join(work, ".claude", "plugins", "from-claude"), "shared")
	installPlugin(t, filepath.Join(work, ".agents", "plugins", "from-agents"), "shared")
	installPlugin(t, filepath.Join(work, ".wingman", "plugins", "from-wingman"), "shared")

	plugins, diagnostics := Discover(work, t.TempDir(), t.TempDir())

	if len(plugins) != 1 {
		t.Fatalf("plugins = %#v, want one", plugins)
	}

	if !strings.Contains(plugins[0].Root, "from-wingman") {
		t.Fatalf("winner = %q, want the .wingman copy", plugins[0].Root)
	}

	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics = %v, want both losers reported", diagnostics)
	}
}

func TestDiscoverPrefersProjectOverPersonal(t *testing.T) {
	_, wingmanHome := isolateHome(t)
	work := t.TempDir()

	installPlugin(t, filepath.Join(wingmanHome, "plugins", "personal"), "shared")
	installPlugin(t, filepath.Join(work, ".claude", "plugins", "project"), "shared")

	plugins, _ := Discover(work, t.TempDir(), t.TempDir())

	if len(plugins) != 1 || !strings.Contains(plugins[0].Root, "project") {
		t.Fatalf("plugins = %#v, want the project copy", plugins)
	}
}

func TestDiscoverSkipsDirectoriesWithoutManifest(t *testing.T) {
	isolateHome(t)

	work := t.TempDir()

	if err := os.MkdirAll(filepath.Join(work, ".wingman", "plugins", "not-a-plugin"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	plugins, diagnostics := Discover(work, t.TempDir(), t.TempDir())

	if len(plugins) != 0 || len(diagnostics) != 0 {
		t.Fatalf("plugins = %#v, diagnostics = %v, want both empty", plugins, diagnostics)
	}
}

func TestDiscoverReportsInvalidPluginAndKeepsGoing(t *testing.T) {
	isolateHome(t)

	work := t.TempDir()
	root := filepath.Join(work, ".wingman", "plugins")

	installPlugin(t, filepath.Join(root, "good"), "good")

	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, "plugin.json"), []byte(`{"name": "Broken"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	plugins, diagnostics := Discover(work, t.TempDir(), t.TempDir())

	if len(plugins) != 1 || plugins[0].Name != "good" {
		t.Fatalf("plugins = %#v, want only the valid one", plugins)
	}

	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "$schema") {
		t.Fatalf("diagnostics = %v, want the broken plugin reported", diagnostics)
	}
}

func TestServersResolveEarliestPluginFirst(t *testing.T) {
	plugins := []Plugin{
		{Name: "first", Root: "/a", Servers: map[string]mcp.ServerConfig{
			"shared": {URL: "https://first.example/mcp"},
		}},
		{Name: "second", Root: "/b", Servers: map[string]mcp.ServerConfig{
			"shared": {URL: "https://second.example/mcp"},
			"own":    {URL: "https://second.example/own"},
		}},
	}

	servers, diagnostics := Servers(plugins)

	if len(servers) != 2 {
		t.Fatalf("servers = %#v, want two", servers)
	}

	if servers["shared"].URL != "https://first.example/mcp" {
		t.Fatalf("shared = %#v, want the first plugin's copy", servers["shared"])
	}

	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "shadowed") {
		t.Fatalf("diagnostics = %v, want the collision reported", diagnostics)
	}
}

func TestSkillsPreserveDiscoveryOrder(t *testing.T) {
	plugins := []Plugin{
		{Name: "a", Skills: []skill.Skill{{Name: "one", Plugin: "a"}}},
		{Name: "b", Skills: []skill.Skill{{Name: "two", Plugin: "b"}}},
	}

	skills := Skills(plugins)

	if len(skills) != 2 || skills[0].Name != "one" || skills[1].Name != "two" {
		t.Fatalf("skills = %#v", skills)
	}
}

func TestLanguageServersComeFromPersonalPluginsOnly(t *testing.T) {
	_, wingmanHome := isolateHome(t)
	work := t.TempDir()

	definition := func(name string) []byte {
		return []byte(`{"` + name + `": {"command": "` + name + `-ls", "args": ["--stdio"], "extensionToLanguage": {".zig": "zig"}}}`)
	}
	project := filepath.Join(work, ".wingman", "plugins", "project")
	personal := filepath.Join(wingmanHome, "plugins", "personal")
	later := filepath.Join(wingmanHome, "plugins", "sooner-loses")
	broken := filepath.Join(wingmanHome, "plugins", "broken")
	for dir, name := range map[string]string{project: "project", personal: "personal", later: "sooner-loses", broken: "broken"} {
		installPlugin(t, dir, name)
	}
	for path, content := range map[string][]byte{
		filepath.Join(project, ".lsp.json"):  definition("repo"),
		filepath.Join(personal, ".lsp.json"): definition("zls"),
		filepath.Join(later, ".lsp.json"):    definition("other"),
		filepath.Join(broken, ".lsp.json"):   []byte(`{"bad": {"command": "bad", "extensionToLanguage": {".x": "x"}, "surprise": true}}`),
	} {
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatal(err)
		}
	}

	plugins, diagnostics := Discover(work, t.TempDir(), t.TempDir())
	if len(plugins) != 4 || !strings.Contains(strings.Join(diagnosticMessages(diagnostics), "\n"), `unknown field "surprise"`) {
		t.Fatalf("plugins = %d, diagnostics = %v; want all loaded and the broken file reported", len(plugins), diagnostics)
	}

	servers, notes := LanguageServers(plugins, work)
	if len(servers) != 2 || servers[0].Name != "zls" || servers[0].Command != "zls-ls" || servers[1].Name != "other" {
		t.Fatalf("servers = %+v, want the personal plugins' in discovery order", servers)
	}
	messages := strings.Join(diagnosticMessages(notes), "\n")
	for _, want := range []string{"language servers of a project plugin are not started", `language server "other" is not used for .zig files; "zls" handles them`} {
		if !strings.Contains(messages, want) {
			t.Errorf("notes = %q, want %q", messages, want)
		}
	}
}

func TestLanguageServersFromManifestAndVariables(t *testing.T) {
	_, wingmanHome := isolateHome(t)
	work := t.TempDir()
	dir := filepath.Join(wingmanHome, "plugins", "servers")
	files := map[string]string{
		"plugin.json": `{"$schema": "` + ManifestSchema + `", "name": "servers", "lspServers": [
			"./extra/more.json",
			{"zls": {"command": "${CLAUDE_PLUGIN_ROOT}/bin/zls", "args": ["--root", "${CLAUDE_PROJECT_DIR}"],
				"env": {"CACHE": "${PLUGIN_DATA}/cache"}, "workspaceFolder": "${CLAUDE_PROJECT_DIR}/zig",
				"extensionToLanguage": {".zig": "zig"}},
			 "a-later": {"command": "a-later", "extensionToLanguage": {".zig": "zig"}}},
			"../outside.json"
		]}`,
		".lsp.json":       `{"zls": {"command": "replaced", "extensionToLanguage": {".zig": "zig"}}, "kept": {"command": "kept-ls", "extensionToLanguage": {".k": "k"}}}`,
		"extra/more.json": `{"more": {"command": "more-ls", "extensionToLanguage": {".m": "m"}}}`,
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	plugins, diagnostics := Discover(work, t.TempDir(), t.TempDir())
	if len(plugins) != 1 {
		t.Fatalf("plugins = %v, diagnostics = %v", plugins, diagnostics)
	}
	messages := strings.Join(diagnosticMessages(diagnostics), "\n")
	if strings.Contains(messages, "unknown manifest field") || !strings.Contains(messages, "../outside.json") {
		t.Fatalf("diagnostics = %q, want only the escaping path reported", messages)
	}

	servers, conflicts := LanguageServers(plugins, work)
	if len(servers) != 4 || servers[0].Name != "zls" || servers[1].Name != "kept" || servers[2].Name != "more" || servers[3].Name != "a-later" {
		t.Fatalf("servers = %+v, want .lsp.json's order, then the manifest's additions", servers)
	}
	// The first registered server keeps the extension, whatever the names sort to.
	if got := strings.Join(diagnosticMessages(conflicts), "\n"); !strings.Contains(got, `language server "a-later" is not used for .zig files; "zls" handles them`) {
		t.Fatalf("conflicts = %q", got)
	}
	root, data := plugins[0].Root, plugins[0].Data
	zls := servers[0]
	if zls.Command != root+"/bin/zls" || zls.Args[1] != work || zls.WorkspaceFolder != work+"/zig" {
		t.Fatalf("the manifest's server did not replace .lsp.json's with variables resolved: %+v", zls)
	}
	for name, want := range map[string]string{"CACHE": data + "/cache", "CLAUDE_PLUGIN_ROOT": root, "PLUGIN_ROOT": root, "CLAUDE_PLUGIN_DATA": data, "CLAUDE_PROJECT_DIR": work} {
		if zls.Env[name] != want {
			t.Errorf("env %s = %q, want %q", name, zls.Env[name], want)
		}
	}
}

func TestClaudeCodePluginLayoutProvidesLanguageServers(t *testing.T) {
	_, wingmanHome := isolateHome(t)
	work := t.TempDir()
	plugins := filepath.Join(wingmanHome, "plugins")
	files := map[string]string{
		// As the official plugins are laid out: a manifest in .claude-plugin, no root plugin.json.
		"gopls-lsp/.claude-plugin/plugin.json": `{"name": "go-tools", "version": "1.0.0", "author": {"name": "x"},
			"lspServers": {"gopls": {"command": "gopls", "extensionToLanguage": {".go": "go"}}}}`,
		"gopls-lsp/.lsp.json": `{"templ": {"command": "${CLAUDE_PLUGIN_ROOT}/templ", "extensionToLanguage": {".templ": "templ"}}}`,
		// Claude Code also accepts a plugin that is only this file.
		"zig/.lsp.json": `{"zls": {"command": "zls", "extensionToLanguage": {".zig": "zig"}}}`,
		// Neither layout, and a Claude Code plugin without language servers.
		"notes/README.md":                   "nothing",
		"skills/.claude-plugin/plugin.json": `{"name": "skills"}`,
	}
	for name, content := range files {
		path := filepath.Join(plugins, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	found, diagnostics := Discover(work, t.TempDir(), t.TempDir())
	if len(found) != 2 || found[0].Name != "go-tools" || found[1].Name != "zig" || len(diagnostics) != 0 {
		t.Fatalf("plugins = %+v, diagnostics = %v", found, diagnostics)
	}
	servers, notes := LanguageServers(found, work)
	if len(servers) != 3 || servers[0].Name != "templ" || servers[1].Name != "gopls" || servers[2].Name != "zls" || len(notes) != 0 {
		t.Fatalf("servers = %+v, notes = %v", servers, notes)
	}
	if servers[0].Command != found[0].Root+"/templ" || servers[0].Env["CLAUDE_PLUGIN_DATA"] == "" {
		t.Fatalf("plugin variables were not resolved: %+v", servers[0])
	}
}

func diagnosticMessages(diagnostics []Diagnostic) []string {
	messages := make([]string, len(diagnostics))
	for i, diagnostic := range diagnostics {
		messages[i] = diagnostic.Message
	}
	return messages
}
