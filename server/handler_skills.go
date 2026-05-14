package server

import (
	"net/http"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/skill"
)

type SkillEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	WhenToUse   string   `json:"when_to_use,omitempty"`
	Arguments   []string `json:"arguments,omitempty"`
}

func (s *Server) resolveSkill(text string) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}

	parts := strings.SplitN(text[1:], " ", 2)
	name := parts[0]
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	ws := s.workspace
	sk := skill.FindSkill(name, ws.Skills)
	if sk == nil {
		return text
	}

	if sk.Bundled {
		_, _ = skill.MaterializeBundled(sk)
	}

	content, err := sk.GetContent(ws.RootPath)
	if err != nil {
		return text
	}

	return sk.ApplyArguments(content, args, sk.AbsoluteDir(ws.RootPath))
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	skills := s.workspace.Skills
	result := make([]SkillEntry, 0, len(skills))
	for _, sk := range skills {
		result = append(result, SkillEntry{
			Name:        sk.Name,
			Description: sk.Description,
			WhenToUse:   sk.WhenToUse,
			Arguments:   sk.Arguments,
		})
	}

	writeJSON(w, result)
}
