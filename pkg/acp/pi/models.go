package pi

import (
	"encoding/json"
	"slices"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
)

const (
	modelConfigID  = "model"
	effortConfigID = "effort"
)

var fallbackThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

const defaultThinkingLevel = "medium"

type modelEntry struct {
	ID   string
	Name string
}

func parseAvailableModels(data json.RawMessage) []modelEntry {
	var d struct {
		Models []struct {
			Provider string `json:"provider"`
			ID       string `json:"id"`
			Name     string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &d) != nil {
		return nil
	}

	out := make([]modelEntry, 0, len(d.Models))
	for _, m := range d.Models {
		provider := strings.TrimSpace(m.Provider)
		id := strings.TrimSpace(m.ID)
		if provider == "" || id == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = id
		}
		out = append(out, modelEntry{
			ID:   provider + "/" + id,
			Name: provider + "/" + name,
		})
	}
	return out
}

type piState struct {
	SessionID     string `json:"sessionId"`
	ThinkingLevel string `json:"thinkingLevel"`
	Model         struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
	} `json:"model"`
}

func parseState(data json.RawMessage) piState {
	var s piState
	_ = json.Unmarshal(data, &s)
	return s
}

func parseAvailableThinkingLevels(data json.RawMessage) []string {
	var d struct {
		Levels []string `json:"levels"`
	}
	if json.Unmarshal(data, &d) != nil {
		return nil
	}

	out := make([]string, 0, len(d.Levels))
	seen := make(map[string]bool, len(d.Levels))
	for _, level := range d.Levels {
		level = strings.TrimSpace(level)
		if level == "" || seen[level] {
			continue
		}
		seen[level] = true
		out = append(out, level)
	}
	return out
}

func (s piState) currentModel() string {
	if s.Model.Provider == "" || s.Model.ID == "" {
		return ""
	}
	return s.Model.Provider + "/" + s.Model.ID
}

func (s piState) thinking() string {
	if s.ThinkingLevel != "" {
		return s.ThinkingLevel
	}
	return defaultThinkingLevel
}

func isThinkingLevel(levels []string, v string) bool {
	return slices.Contains(levels, v)
}

func findModel(models []modelEntry, id string) *modelEntry {
	for i := range models {
		if models[i].ID == id {
			return &models[i]
		}
	}
	return nil
}

func buildConfigOptions(models []modelEntry, currentModel string, thinkingLevels []string, thinking string) []acpsdk.SessionConfigOption {
	opts := make([]acpsdk.SessionConfigOption, 0, 2)
	if len(models) > 0 {
		opts = append(opts, modelConfigOption(models, currentModel))
	}
	if len(thinkingLevels) > 0 {
		opts = append(opts, effortConfigOption(thinkingLevels, thinking))
	}
	return opts
}

func modelConfigOption(models []modelEntry, currentID string) acpsdk.SessionConfigOption {
	ungrouped := make(acpsdk.SessionConfigSelectOptionsUngrouped, 0, len(models))
	for _, m := range models {
		ungrouped = append(ungrouped, acpsdk.SessionConfigSelectOption{
			Value: acpsdk.SessionConfigValueId(m.ID),
			Name:  m.Name,
		})
	}
	if findModel(models, currentID) == nil && len(models) > 0 {
		currentID = models[0].ID
	}
	opt := acpsdk.NewSessionConfigOptionSelect(
		acpsdk.SessionConfigValueId(currentID),
		acpsdk.SessionConfigSelectOptions{Ungrouped: &ungrouped},
	)
	opt.Select.Id = modelConfigID
	opt.Select.Name = "Model"
	return opt
}

func effortConfigOption(levels []string, thinking string) acpsdk.SessionConfigOption {
	if !isThinkingLevel(levels, thinking) {
		thinking = levels[0]
	}
	ungrouped := make(acpsdk.SessionConfigSelectOptionsUngrouped, 0, len(levels))
	for _, l := range levels {
		ungrouped = append(ungrouped, acpsdk.SessionConfigSelectOption{
			Value: acpsdk.SessionConfigValueId(l),
			Name:  acp.TitleCase(l),
		})
	}
	opt := acpsdk.NewSessionConfigOptionSelect(
		acpsdk.SessionConfigValueId(thinking),
		acpsdk.SessionConfigSelectOptions{Ungrouped: &ungrouped},
	)
	desc := "Reasoning effort for this session"
	cat := acpsdk.SessionConfigOptionCategoryThoughtLevel
	opt.Select.Id = effortConfigID
	opt.Select.Name = "Thinking"
	opt.Select.Description = &desc
	opt.Select.Category = &cat
	return opt
}
