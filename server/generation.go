package server

import (
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/model"
)

// resolveGenerationTarget resolves a role ("" = the session's main model,
// "utility", "complex") to a concrete model plus its lowest supported
// reasoning effort — short generations need no deliberation, so the cheapest
// effort wins. Unknown capability keeps effort unset rather than guessing.
func resolveGenerationTarget(cfg *harness.Config, role, override string) (modelID, effort string) {
	modelID = strings.TrimSpace(override)
	var efforts []string
	if modelID == "" && cfg.RoleModel != nil {
		if option, ok := cfg.RoleModel(role); ok && option.ID != "" {
			modelID = strings.TrimSpace(option.ID)
			efforts = option.Efforts
		}
	}
	if modelID == "" && (role == "" || role == "utility") && cfg.Model != nil {
		modelID = strings.TrimSpace(cfg.Model())
	}
	if len(efforts) == 0 {
		if modelEntry, ok := model.Find(modelID); ok {
			efforts = modelEntry.Efforts
		}
	}
	if len(efforts) > 0 {
		effort = efforts[0]
	}
	return modelID, effort
}
