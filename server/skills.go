package server

import (
	"github.com/adrianliechti/wingman-agent/pkg/skill"
)

type SkillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputHint   string `json:"input_hint,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

// skillBlocks expands leading or inline /skill and $skill invocations into
// hidden instruction blocks.
func (s *Project) skillBlocks(text string) []string {
	if s.workspace == nil {
		return nil
	}
	s.refreshSkills()
	var blocks []string
	for _, inv := range skill.Invocations(text, s.workspace.Skills()) {
		block, err := inv.Instructions(s.workspace.RootPath)
		if err != nil {
			continue
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// The catalogue belongs to the workspace. Listing it never starts a backend.
func (s *Project) skillCatalogue() []SkillEntry {
	if s.workspace == nil {
		return []SkillEntry{}
	}
	s.refreshSkills()
	skills := s.workspace.Skills()
	result := make([]SkillEntry, 0, len(skills))
	for _, sk := range skills {
		hint := sk.InvocationHint()
		if hint == "" {
			hint = "[optional arguments]"
		}
		result = append(result, SkillEntry{
			Name:        sk.Qualified(),
			Description: sk.Description,
			InputHint:   hint,
			Scope:       sk.Scope(),
		})
	}
	return result
}
