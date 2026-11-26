package code

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/plugin"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/markdown"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (a *App) showPlugins() {
	plugins := a.agent.Workspace().Plugins
	if len(plugins) == 0 {
		a.appendChat(cellNotice("No plugins installed. Place one in .wingman/plugins/ or ~/.wingman/plugins/.", theme.Default.Yellow, a.width()))
		return
	}

	a.showInfo(func(width int) (string, []string) {
		return fmt.Sprintf("Plugins · %d installed", len(plugins)), pluginLines(plugins, width)
	})
}

func pluginLines(plugins []plugin.Plugin, width int) []string {
	var lines []string
	inner := max(width-len(cellIndent)-3, 10)

	for _, p := range plugins {
		if len(lines) > 0 {
			lines = append(lines, "")
		}

		head := bold(markdown.Sanitize(p.Name))
		if p.Manifest.Version != "" {
			head += " " + dim(markdown.Sanitize(p.Manifest.Version))
		}
		scope := "project"
		if p.Personal {
			scope = "personal"
		}
		lines = append(lines, cellIndent+head+"  "+dim(scope))

		if description := strings.Join(strings.Fields(markdown.Sanitize(p.Manifest.Description)), " "); description != "" {
			for _, line := range ansi.Wrap(description, inner) {
				lines = append(lines, cellIndent+"  "+line)
			}
		}
		if brings := pluginBrings(p); brings != "" {
			lines = append(lines, cellIndent+"  "+dim(brings))
		}
	}

	return lines
}

func pluginBrings(p plugin.Plugin) string {
	count := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}

	var parts []string
	if n := len(p.Skills); n > 0 {
		parts = append(parts, count(n, "skill"))
	}
	if n := len(p.Servers); n > 0 {
		parts = append(parts, count(n, "MCP server"))
	}
	if n := len(p.LanguageServers); n > 0 {
		parts = append(parts, count(n, "language server"))
	}
	if p.Hooks.RuleCount() > 0 {
		parts = append(parts, "hooks")
	}
	return strings.Join(parts, " · ")
}
