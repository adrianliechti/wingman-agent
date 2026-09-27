package codex

import (
	"regexp"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"

	acpcommon "github.com/adrianliechti/wingman-agent/pkg/acp"
)

type modelEntry struct {
	ID            string
	Name          string
	Description   string
	EffortLevels  []string
	DefaultEffort string
	Default       bool
}

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
			ID:            m.ID,
			Name:          formatModelDisplayName(name),
			Description:   m.Description,
			EffortLevels:  efforts,
			DefaultEffort: m.DefaultReasoningEffort,
			Default:       m.IsDefault,
		})
	}
	return out
}

var (
	gptPrefixRe        = regexp.MustCompile(`(?i)^gpt-`)
	modelNameSeparator = regexp.MustCompile(`[-/]+`)
)

// formatModelDisplayName turns Codex's GPT ids into compact picker labels
// ("gpt-5.3-codex" -> "5.3 Codex"); other provider names keep their punctuation.
func formatModelDisplayName(name string) string {
	if !gptPrefixRe.MatchString(name) {
		return name
	}
	var parts []string
	for _, part := range modelNameSeparator.Split(name[len("gpt-"):], -1) {
		if part != "" {
			parts = append(parts, strings.ToUpper(part[:1])+part[1:])
		}
	}
	return strings.Join(parts, " ")
}

func findModel(models []modelEntry, id string) *modelEntry {
	for i := range models {
		if models[i].ID == id {
			return &models[i]
		}
	}
	return nil
}

func resolveModel(models []modelEntry, id string) *modelEntry {
	if id != "" && id != "default" {
		return findModel(models, id)
	}
	for i := range models {
		if models[i].Default {
			return &models[i]
		}
	}
	return nil
}

const (
	modelConfigID             = "model"
	effortConfigID            = "effort"
	collaborationModeConfigID = "collaboration_mode"

	defaultCollaborationMode = "default"
	planCollaborationMode    = "plan"
)

// recommend adds the negotiated AIR recommendedValue: the catalog default
// model and the current model's default effort, independent of the selection.
func buildConfigOptions(models []modelEntry, currentModelID, currentEffort, collaborationMode string, recommend bool) []acp.SessionConfigOption {
	model := modelConfigOption(models, currentModelID)
	effort := effortConfigOption(models, currentModelID, currentEffort)
	if recommend {
		if m := resolveModel(models, "default"); m != nil {
			model.Select.Meta = airMeta(recommendedValueCapability, m.ID)
		}
		if m := findModel(models, currentModelID); m != nil && effort != nil && slices.Contains(m.EffortLevels, m.DefaultEffort) {
			effort.Select.Meta = airMeta(recommendedValueCapability, m.DefaultEffort)
		}
	}
	opts := []acp.SessionConfigOption{model}
	if effort != nil {
		opts = append(opts, *effort)
	}
	opts = append(opts, collaborationModeConfigOption(collaborationMode))
	return opts
}

func collaborationModeConfigOption(current string) acp.SessionConfigOption {
	if current != planCollaborationMode {
		current = defaultCollaborationMode
	}
	ungrouped := acp.SessionConfigSelectOptionsUngrouped{
		{Value: defaultCollaborationMode, Name: "Default"},
		{
			Value:       planCollaborationMode,
			Name:        "Plan",
			Description: new("Plan before making changes"),
		},
	}
	opt := acp.NewSessionConfigOptionSelect(
		acp.SessionConfigValueId(current),
		acp.SessionConfigSelectOptions{Ungrouped: &ungrouped},
	)
	desc := "How Codex collaborates for subsequent turns"
	category := acp.SessionConfigOptionCategory("_codex_collaboration_mode")
	opt.Select.Id = collaborationModeConfigID
	opt.Select.Name = "Collaboration mode"
	opt.Select.Description = &desc
	opt.Select.Category = &category
	return opt
}

func resumedCollaborationMode(mode *codexCollaborationMode) string {
	if mode != nil && mode.Mode == planCollaborationMode {
		return planCollaborationMode
	}
	return defaultCollaborationMode
}

func isValidCollaborationMode(mode string) bool {
	return mode == defaultCollaborationMode || mode == planCollaborationMode
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
	if currentID == "" && len(models) > 0 {
		currentID = models[0].ID
	}
	if currentID != "" && findModel(models, currentID) == nil {
		ungrouped = append(acp.SessionConfigSelectOptionsUngrouped{
			{Value: acp.SessionConfigValueId(currentID), Name: formatModelDisplayName(currentID)},
		}, ungrouped...)
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
	if m == nil {
		return nil
	}
	return acpcommon.EffortConfigOption(m.EffortLevels, currentEffort)
}

func normalizeSessionConfig(models []modelEntry, modelID, effort string) (string, string) {
	if m := resolveModel(models, modelID); m != nil {
		modelID = m.ID
		if !acpcommon.IsValidEffort(m.EffortLevels, effort) {
			effort = "default"
		}
	}
	return modelID, effort
}

const defaultModeID = "agent"

type sessionMode struct {
	id             string
	name           string
	description    string
	approvalPolicy any
	sandboxPolicy  any
}

var sessionModes = []sessionMode{
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
		id:             "plan",
		name:           "Plan",
		description:    "Read-only — proposes a plan, doesn't edit code.",
		approvalPolicy: "on-request",
		sandboxPolicy:  map[string]any{"type": "readOnly", "networkAccess": false},
	},
	{
		id:             "unattended",
		name:           "Unattended",
		description:    "Runs with full access and without permission prompts.",
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
	infos := make([]acpcommon.ModeInfo, 0, len(sessionModes))
	for _, m := range sessionModes {
		infos = append(infos, acpcommon.ModeInfo{ID: m.id, Name: m.name, Description: m.description})
	}
	return acpcommon.SessionModeState(infos, currentID, defaultModeID)
}
