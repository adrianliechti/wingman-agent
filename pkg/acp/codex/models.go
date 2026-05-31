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

const defaultModeID = "agent"

// sessionMode maps an ACP mode id to the codex approval + sandbox policy sent
// on turn/start. Policies are untyped JSON; the SDK exposes no typed enum.
type sessionMode struct {
	id             string
	name           string
	description    string
	approvalPolicy any
	sandboxPolicy  any
}

var sessionModes = []sessionMode{
	{
		id:             "read-only",
		name:           "Read-only",
		description:    "Read files only. Editing or running commands needs approval.",
		approvalPolicy: "on-request",
		sandboxPolicy:  map[string]any{"type": "readOnly", "networkAccess": false},
	},
	{
		id:             "agent",
		name:           "Agent",
		description:    "Read and edit files and run commands. Asks before acting outside the workspace.",
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
		id:             "full-access",
		name:           "Full Access",
		description:    "Edit files anywhere and run commands with network access, without asking.",
		approvalPolicy: "never",
		sandboxPolicy:  map[string]any{"type": "dangerFullAccess"},
	},
}

func findMode(id string) *sessionMode {
	for i := range sessionModes {
		if sessionModes[i].id == id {
			return &sessionModes[i]
		}
	}
	return nil
}

func modeFor(id string) sessionMode {
	if m := findMode(id); m != nil {
		return *m
	}
	return *findMode(defaultModeID)
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
