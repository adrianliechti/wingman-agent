package code

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/skill"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/markdown"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (a *App) showSkills() {
	workspace := a.agent.Workspace()
	a.requestSkillRefresh()
	if len(workspace.Skills()) == 0 {
		a.appendChat(cellNotice("No skills available", theme.Default.Yellow, a.width()))
		return
	}

	a.showInfo(func(width int) (string, []string) {
		skills := workspace.Skills()
		return fmt.Sprintf("Skills · %d available", len(skills)), skillLines(skills, width)
	})
}

// A project's and the user's own skills come before the ones Wingman brings.
var skillScopes = []struct{ scope, label string }{
	{"project", "project"},
	{"personal", "personal"},
	{"plugin", "plugin"},
	{"builtin", "built-in"},
}

func skillLines(skills []skill.Skill, width int) []string {
	var lines []string

	for _, group := range skillScopes {
		heading := false

		for i := range skills {
			s := &skills[i]
			if s.Scope() != group.scope {
				continue
			}
			if !heading {
				if len(lines) > 0 {
					lines = append(lines, "")
				}
				lines = append(lines, cellIndent+bold(group.label))
				heading = true
			}

			head := "/" + markdown.Sanitize(skillCommandName(s, skills))
			if hint := s.InvocationHint(); hint != "" {
				head += " " + dim(markdown.Sanitize(hint))
			}
			if s.Plugin != "" {
				head += "  " + dim(markdown.Sanitize(s.Plugin))
			}
			lines = append(lines, cellIndent+"  "+head)

			description := strings.Join(strings.Fields(markdown.Sanitize(s.Description)), " ")
			if description == "" {
				continue
			}
			for _, line := range ansi.Wrap(dim(description), max(width-len(cellIndent)-5, 10)) {
				lines = append(lines, cellIndent+"    "+line)
			}
		}
	}

	return lines
}
