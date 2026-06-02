package claude

import (
	"strings"

	"github.com/coder/acp-go-sdk"
)

// ModelEntry describes a Claude model we expose to ACP clients along with
// the effort levels the CLI accepts when that model is selected.
type ModelEntry struct {
	ID           string
	Name         string
	Description  string
	EffortLevels []string // empty == effort selector hidden
}

// The model list is fetched at runtime from the `claude` CLI's stdio control
// protocol (see fetchModels); there is no static fallback table.

func findModel(models []ModelEntry, id string) *ModelEntry {
	for i := range models {
		if models[i].ID == id {
			return &models[i]
		}
	}
	return nil
}

// resolveModel maps a client-supplied id to a known model, tolerating friendly
// aliases ("opus", "opus[1m]") via case-insensitive id/name substring matching
// after the exact-id lookup fails. Returns nil when nothing plausibly matches.
func resolveModel(models []ModelEntry, id string) *ModelEntry {
	if m := findModel(models, id); m != nil {
		return m
	}
	want := strings.ToLower(strings.TrimSpace(id))
	if want == "" {
		return nil
	}
	for i := range models {
		if strings.EqualFold(models[i].Name, id) {
			return &models[i]
		}
	}
	for i := range models {
		lid, lname := strings.ToLower(models[i].ID), strings.ToLower(models[i].Name)
		if strings.Contains(lid, want) || strings.Contains(lname, want) {
			return &models[i]
		}
	}
	return nil
}

const (
	modelConfigID  = "model"
	effortConfigID = "effort"
)

// buildConfigOptions returns the model selector plus, when the active model
// supports it, the effort selector. Since v0.13.5 model selection flows through
// config options rather than a dedicated SessionModelState.
func buildConfigOptions(models []ModelEntry, currentModelID, currentEffort string) []acp.SessionConfigOption {
	opts := []acp.SessionConfigOption{modelConfigOption(models, currentModelID)}
	if effort := effortConfigOption(models, currentModelID, currentEffort); effort != nil {
		opts = append(opts, *effort)
	}
	return opts
}

func modelConfigOption(models []ModelEntry, currentID string) acp.SessionConfigOption {
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

func effortConfigOption(models []ModelEntry, currentModelID, currentEffort string) *acp.SessionConfigOption {
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
