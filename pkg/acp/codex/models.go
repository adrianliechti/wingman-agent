package codex

import "github.com/coder/acp-go-sdk"

// modelEntry describes one codex model + its supported reasoning effort levels.
// Codex's `model/list` returns this dynamically, but a static table keeps the
// agent functional without that round-trip.
type modelEntry struct {
	ID           string
	Name         string
	Description  string
	EffortLevels []string
}

// codex's ReasoningEffort enum.
var defaultEffortLevels = []string{"minimal", "low", "medium", "high", "xhigh"}

var builtinModels = []modelEntry{
	{ID: "default", Name: "Default", Description: "Use the configured default model"},
	{ID: "gpt-5", Name: "GPT-5", Description: "OpenAI GPT-5", EffortLevels: defaultEffortLevels},
	{ID: "gpt-5-codex", Name: "GPT-5 Codex", Description: "Codex tuning of GPT-5", EffortLevels: defaultEffortLevels},
	{ID: "o3", Name: "o3", Description: "OpenAI o3 reasoning model", EffortLevels: defaultEffortLevels},
	{ID: "o4-mini", Name: "o4-mini", Description: "OpenAI o4-mini reasoning model", EffortLevels: defaultEffortLevels},
}

func findModel(id string) *modelEntry {
	for i := range builtinModels {
		if builtinModels[i].ID == id {
			return &builtinModels[i]
		}
	}
	return nil
}

func buildSessionModelState(currentID string) *acp.SessionModelState {
	infos := make([]acp.ModelInfo, 0, len(builtinModels))
	for _, m := range builtinModels {
		desc := m.Description
		infos = append(infos, acp.ModelInfo{
			ModelId:     acp.ModelId(m.ID),
			Name:        m.Name,
			Description: &desc,
		})
	}
	if findModel(currentID) == nil {
		currentID = "default"
	}
	return &acp.SessionModelState{
		AvailableModels: infos,
		CurrentModelId:  acp.ModelId(currentID),
	}
}

func buildConfigOptions(currentModelID, currentEffort string) []acp.SessionConfigOption {
	m := findModel(currentModelID)
	if m == nil || len(m.EffortLevels) == 0 {
		return nil
	}

	ungrouped := acp.SessionConfigSelectOptionsUngrouped{
		{Value: "default", Name: "Default"},
	}
	for _, lvl := range m.EffortLevels {
		ungrouped = append(ungrouped, acp.SessionConfigSelectOption{
			Value: acp.SessionConfigValueId(lvl),
			Name:  titleCase(lvl),
		})
	}

	current := currentEffort
	if !isValidEffort(m, current) {
		current = "default"
	}
	opt := acp.NewSessionConfigOptionSelect(
		acp.SessionConfigValueId(current),
		acp.SessionConfigSelectOptions{Ungrouped: &ungrouped},
	)
	desc := "Reasoning effort for the selected model"
	cat := acp.SessionConfigOptionCategoryThoughtLevel
	opt.Select.Id = "effort"
	opt.Select.Name = "Effort"
	opt.Select.Description = &desc
	opt.Select.Category = &cat
	return []acp.SessionConfigOption{opt}
}

func isValidEffort(m *modelEntry, level string) bool {
	if level == "" || level == "default" {
		return true
	}
	for _, l := range m.EffortLevels {
		if l == level {
			return true
		}
	}
	return false
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	out := []byte(s)
	if out[0] >= 'a' && out[0] <= 'z' {
		out[0] -= 0x20
	}
	return string(out)
}

// --- session modes ---

// agentMode bundles a codex approval policy + sandbox policy under an ACP
// session mode id. The Go SDK doesn't currently expose a typed enum for these,
// so we send the codex shapes as untyped JSON.
type agentMode struct {
	id             string
	name           string
	description    string
	approvalPolicy any
	sandboxPolicy  any
}

var agentModes = []agentMode{
	{
		id:             "read-only",
		name:           "Read-only",
		description:    "Requires approval to edit files and run commands.",
		approvalPolicy: "on-request",
		sandboxPolicy:  map[string]any{"type": "readOnly", "networkAccess": false},
	},
	{
		id:             "agent",
		name:           "Agent",
		description:    "Read and edit files, and run commands.",
		approvalPolicy: "on-request",
		sandboxPolicy: map[string]any{
			"type":                "workspaceWrite",
			"writableRoots":       []string{},
			"networkAccess":       false,
			"excludeTmpdirEnvVar": false,
			"excludeSlashTmp":     false,
		},
	},
	{
		id:             "agent-full-access",
		name:           "Agent (full access)",
		description:    "Edit files outside this workspace and run commands with network access.",
		approvalPolicy: "never",
		sandboxPolicy:  map[string]any{"type": "dangerFullAccess"},
	},
}

func findMode(id string) *agentMode {
	for i := range agentModes {
		if agentModes[i].id == id {
			return &agentModes[i]
		}
	}
	return nil
}

func agentModeFor(id string) agentMode {
	if m := findMode(id); m != nil {
		return *m
	}
	return *findMode("agent")
}

func buildSessionModeState(currentID string) *acp.SessionModeState {
	if currentID == "" {
		currentID = "agent"
	}
	available := make([]acp.SessionMode, 0, len(agentModes))
	for _, m := range agentModes {
		desc := m.description
		available = append(available, acp.SessionMode{
			Id:          acp.SessionModeId(m.id),
			Name:        m.name,
			Description: &desc,
		})
	}
	return &acp.SessionModeState{
		AvailableModes: available,
		CurrentModeId:  acp.SessionModeId(currentID),
	}
}
