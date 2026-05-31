package claude

import "github.com/coder/acp-go-sdk"

// ModelEntry describes a Claude model we expose to ACP clients along with
// the effort levels the CLI accepts when that model is selected.
type ModelEntry struct {
	ID           string
	Name         string
	Description  string
	EffortLevels []string // empty == effort selector hidden
}

// defaultEffortLevels are the values `claude --effort <level>` accepts.
var defaultEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// builtinModels is the static list surfaced to the client. The CLI accepts
// these aliases (and date-pinned IDs) via --model.
var builtinModels = []ModelEntry{
	{ID: "default", Name: "Default", Description: "Use the CLI's configured default model"},
	{ID: "opus", Name: "Claude Opus", Description: "Most capable; best for complex reasoning", EffortLevels: defaultEffortLevels},
	{ID: "sonnet", Name: "Claude Sonnet", Description: "Balanced speed and capability", EffortLevels: defaultEffortLevels},
	{ID: "haiku", Name: "Claude Haiku", Description: "Fastest, lowest cost", EffortLevels: defaultEffortLevels},
	{ID: "opusplan", Name: "Opus (Plan)", Description: "Opus while planning, Sonnet for execution", EffortLevels: defaultEffortLevels},
}

func findModel(id string) *ModelEntry {
	for i := range builtinModels {
		if builtinModels[i].ID == id {
			return &builtinModels[i]
		}
	}
	return nil
}

// buildSessionModelState returns the model selector state advertised in
// NewSessionResponse.Models.
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

// buildConfigOptions returns the effort selector for the active model.
// The model and mode selectors live in their own SessionModelState /
// SessionModeState fields, so the only config option we emit is "effort".
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

func isValidEffort(m *ModelEntry, level string) bool {
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

// Mode ids are passed verbatim to `claude --permission-mode`. Tool approvals in
// default/acceptEdits are surfaced via the stdio control protocol (see approver).
const defaultModeID = "default"

type sessionMode struct {
	id          string
	name        string
	description string
}

var sessionModes = []sessionMode{
	{id: "default", name: "Agent", description: "Asks before editing files or running commands."},
	{id: "acceptEdits", name: "Accept Edits", description: "Auto-accepts file edits; asks before running commands."},
	{id: "plan", name: "Plan", description: "Read-only — proposes a plan, doesn't edit code."},
	{id: "bypassPermissions", name: "Full Access", description: "Edit files and run commands without asking."},
}

func findMode(id string) *sessionMode {
	for i := range sessionModes {
		if sessionModes[i].id == id {
			return &sessionModes[i]
		}
	}
	return nil
}

func buildSessionModeState(currentID string) *acp.SessionModeState {
	if currentID == "" {
		currentID = defaultModeID
	}
	available := make([]acp.SessionMode, 0, len(sessionModes))
	for _, m := range sessionModes {
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
