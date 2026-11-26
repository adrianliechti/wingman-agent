package code

import (
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/external"
	"github.com/adrianliechti/wingman-agent/pkg/plugin"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestSkillLinesGroupBySourceWithOwnSkillsFirst(t *testing.T) {
	skills := []skill.Skill{
		{Name: "commit", Description: "Create a\ncommit", Bundled: true},
		{Name: "check", Description: "Run the release checklist", Plugin: "release-tools"},
		{Name: "standup", Description: "Summarize \x1b[31myesterday", Location: "/home/me/.wingman/skills/standup"},
		{Name: "deploy", Description: strings.Repeat("Deploy a preview. ", 8), Location: ".wingman/skills/deploy", ArgumentHint: "[environment]"},
		{Name: "deploy", Description: "A plugin's own deploy", Plugin: "release-tools"},
	}

	lines := skillLines(skills, 60)
	for _, line := range lines {
		if strings.Contains(ansi.Strip(line), "\x1b") {
			t.Fatalf("skill text reached the terminal unsanitized: %q", line)
		}
		if ansi.Width(line) > 60 {
			t.Fatalf("line wider than the view: %q", ansi.Strip(line))
		}
	}

	text := ansi.Strip(strings.Join(lines, "\n"))
	order := []string{
		"project", "/deploy [environment]", "Deploy a preview.",
		"personal", "/standup", "Summarize yesterday",
		"plugin", "/check  release-tools", "/release-tools:deploy  release-tools",
		"built-in", "/commit", "Create a commit",
	}
	at := 0
	for _, want := range order {
		next := strings.Index(text[at:], want)
		if next < 0 {
			t.Fatalf("missing %q after offset %d in:\n%s", want, at, text)
		}
		at += next + len(want)
	}
}

func TestPluginLinesShowWhatEachPluginBrings(t *testing.T) {
	plugins := []plugin.Plugin{
		{
			Name:     "release-tools",
			Manifest: plugin.Manifest{Version: "1.0.0", Description: "Release checklist\nand its servers"},
			Skills:   []skill.Skill{{Name: "check"}},
		},
		{Name: "review", Personal: true, Hooks: &external.Config{}},
	}

	text := ansi.Strip(strings.Join(pluginLines(plugins, 60), "\n"))
	for _, want := range []string{
		"release-tools 1.0.0  project",
		"Release checklist and its servers",
		"1 skill",
		"review  personal",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "hooks") {
		t.Errorf("a plugin without hook rules claims hooks:\n%s", text)
	}
}

func TestSkillsAndPluginsCommandsOpenOverlays(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 80, 12)
	agent := a.agent.(*uiTestAgent)

	for _, name := range []string{"/skills", "/plugins"} {
		if cmd := a.findBuiltin(name); cmd == nil || !cmd.Busy {
			t.Fatalf("%s = %+v, want a command usable during a turn", name, cmd)
		}
	}

	a.showSkills()
	a.showPlugins()
	if a.overlay != nil {
		t.Fatal("overlay opened with nothing to show")
	}
	chat := ansi.Strip(strings.Join(a.chat, "\n"))
	if !strings.Contains(chat, "No skills available") || !strings.Contains(chat, "No plugins installed") {
		t.Fatalf("chat = %q", chat)
	}

	agent.workspace.Plugins = []plugin.Plugin{{Name: "release-tools", Manifest: plugin.Manifest{Version: "1.0.0"}}}
	a.showPlugins()
	if a.overlay == nil {
		t.Fatal("plugins overlay did not open")
	}
	screen := ansi.Strip(strings.Join(a.overlay.Render(80, 12), "\n"))
	for _, want := range []string{"Plugins · 1 installed", "release-tools 1.0.0  project"} {
		if !strings.Contains(screen, want) {
			t.Errorf("missing %q in:\n%s", want, screen)
		}
	}
	if !a.overlay.HandleKey(inline.KeyEvent{Key: inline.KeyEsc}) {
		t.Fatal("esc did not close the overlay")
	}
}
