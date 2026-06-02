package codex

import "github.com/coder/acp-go-sdk"

// modelEntry describes one codex model + its supported reasoning effort levels.
// The list is fetched at runtime from codex's `model/list` RPC (see
// Agent.ensureModels); there is no static fallback table.
type modelEntry struct {
	ID           string
	Name         string
	Description  string
	EffortLevels []string
}

// modelsFromCodex converts a `model/list` response into the picker entries we
// advertise over ACP, dropping models codex marks hidden.
func modelsFromCodex(list []codexModel) []modelEntry {
	out := make([]modelEntry, 0, len(list))
	for _, m := range list {
		if m.Hidden {
			continue
		}
		efforts := make([]string, 0, len(m.SupportedReasoningEfforts))
		for _, e := range m.SupportedReasoningEfforts {
			efforts = append(efforts, e.ReasoningEffort)
		}
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		out = append(out, modelEntry{
			ID:           m.ID,
			Name:         name,
			Description:  m.Description,
			EffortLevels: efforts,
		})
	}
	return out
}

func findModel(models []modelEntry, id string) *modelEntry {
	for i := range models {
		if models[i].ID == id {
			return &models[i]
		}
	}
	return nil
}

const (
	modelConfigID  = "model"
	effortConfigID = "effort"
)

func buildConfigOptions(models []modelEntry, currentModelID, currentEffort string) []acp.SessionConfigOption {
	opts := []acp.SessionConfigOption{modelConfigOption(models, currentModelID)}
	if effort := effortConfigOption(models, currentModelID, currentEffort); effort != nil {
		opts = append(opts, *effort)
	}
	return opts
}

func modelConfigOption(models []modelEntry, currentID string) acp.SessionConfigOption {
	ungrouped := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(models))
	for _, m := range models {
		desc := m.Description
		opt := acp.SessionConfigSelectOption{
			Value: acp.SessionConfigValueId(m.ID),
			Name:  m.Name,
		}
		if desc != "" {
			opt.Description = &desc
		}
		ungrouped = append(ungrouped, opt)
	}
	if findModel(models, currentID) == nil && len(models) > 0 {
		currentID = models[0].ID
	}
	opt := acp.NewSessionConfigOptionSelect(
		acp.SessionConfigValueId(currentID),
		acp.SessionConfigSelectOptions{Ungrouped: &ungrouped},
	)
	opt.Select.Id = modelConfigID
	opt.Select.Name = "Model"
	return opt
}

func effortConfigOption(models []modelEntry, currentModelID, currentEffort string) *acp.SessionConfigOption {
	m := findModel(models, currentModelID)
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
	if current == "" || !isValidEffort(m, current) {
		current = "default"
	}
	opt := acp.NewSessionConfigOptionSelect(
		acp.SessionConfigValueId(current),
		acp.SessionConfigSelectOptions{Ungrouped: &ungrouped},
	)
	desc := "Reasoning effort for the selected model"
	cat := acp.SessionConfigOptionCategoryThoughtLevel
	opt.Select.Id = effortConfigID
	opt.Select.Name = "Effort"
	opt.Select.Description = &desc
	opt.Select.Category = &cat
	return &opt
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
		id:             "agent-full-access",
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
