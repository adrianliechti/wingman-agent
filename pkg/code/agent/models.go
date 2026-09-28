package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/model"
)

func (a *Agent) Models(sessionID string) ([]model.Model, string) {
	s := a.session(sessionID)
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	available := model.Available(a.upstreamModels)
	current := a.selectedModelLocked(s, available)
	if current != "" {
		canonical := model.CanonicalID(current)
		index := slices.IndexFunc(available, func(m model.Model) bool {
			return model.CanonicalID(m.ID) == canonical
		})
		if index >= 0 {
			available[index].ID = current
		} else {
			available = append(available, model.Model{ID: current, Name: model.Name(current)})
		}
	}
	for i := range available {
		available[i].Efforts = slices.Clone(effortValuesFor(available[i].ID)[1:])
	}
	return available, current
}

func (a *Agent) selectedModelLocked(s *sessionState, available []model.Model) string {
	current := a.modelID
	if s != nil && s.modelID != "" {
		current = s.modelID
	}
	if current == "" {
		current = classModel(available, model.ClassMedium, "")
	}
	current = a.availableModel(current, available)
	if current == "" && len(available) > 0 {
		current = available[0].ID
	}
	return current
}

// Model roles are explicit delegation choices, independent of collaboration
// mode. Their automatic picks prefer the selected model's family.
func (a *Agent) roleModelLocked(s *sessionState, name string) (string, bool) {
	available := model.Available(a.upstreamModels)
	current := a.selectedModelLocked(s, available)

	var override string
	var class model.Class
	switch name {
	case "", "default", "main":
		return current, current != ""
	case "complex", "plan": // plan is the legacy delegation role.
		override, class = a.complexModel, model.ClassLarge
		if override == "" && (a.upstreamModels == nil || model.ClassOf(current) == model.ClassLarge) {
			return current, current != ""
		}
	case "utility":
		// Explicit utility deployments need not appear in model discovery.
		if a.utilityModel != "" {
			return a.utilityModel, true
		}
		if a.upstreamModels == nil {
			return "", false
		}
		class = model.ClassSmall
	default:
		return "", false
	}
	if override == "" {
		override = classModel(available, class, model.Family(current))
	}
	if resolved := a.availableModel(override, available); resolved != "" {
		return resolved, true
	}
	return current, current != ""
}

func (a *Agent) availableModel(id string, available []model.Model) string {
	if id == "" || a.upstreamModels == nil || len(available) == 0 || a.upstreamModels[id] {
		return id
	}
	canonical := model.CanonicalID(id)
	for _, candidate := range available {
		if model.CanonicalID(candidate.ID) == canonical {
			return candidate.ID
		}
	}
	return ""
}

func (a *Agent) roleModel(s *sessionState, role string) (harness.ModelOption, bool) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	id, ok := a.roleModelLocked(s, role)
	if !ok {
		return harness.ModelOption{}, false
	}
	m, _ := model.Find(id)
	return harness.ModelOption{ID: id, Efforts: slices.Clone(m.Efforts)}, true
}

// RoleModel resolves "default", "complex", or "utility" without a session.
// Empty and "main" select default; "plan" is a compatibility alias for complex.
func (a *Agent) RoleModel(role string) (harness.ModelOption, bool) {
	return a.roleModel(nil, role)
}

// classModel returns the first available model in the requested class,
// preferring the selected model's family when supplied.
func classModel(available []model.Model, class model.Class, family string) string {
	var fallback string
	for _, m := range available {
		if m.Class != class {
			continue
		}
		if family == "" || model.Family(m.ID) == family {
			return m.ID
		}
		if fallback == "" {
			fallback = m.ID
		}
	}
	return fallback
}

// SetModel selects one model for the session, shared by Agent and Plan modes.
func (a *Agent) SetModel(_ context.Context, sessionID, id string) error {
	s := a.session(sessionID)
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	available := model.Available(a.upstreamModels)
	canonical := model.CanonicalID(id)
	effort := a.requestedEffortLocked(s)
	if model.CanonicalID(a.selectedModelLocked(s, available)) != canonical {
		effort = ""
	}
	if !a.options.IsolateSessionSettings || sessionID == "" {
		if model.CanonicalID(a.selectedModelLocked(nil, available)) != canonical {
			a.effort = ""
		}
		a.modelID = id
	}
	// Reset effort only for a different model. Pin the session's prior choice
	// when reselecting it, since updating the shared default may reset effort.
	if s != nil {
		s.modelID = id
		s.effort = &effort
	}
	return nil
}

func (a *Agent) FetchModels(ctx context.Context) {
	models, err := a.cfg.Models(ctx)
	if err != nil {
		return
	}
	ids := make(map[string]bool, len(models))
	for _, m := range models {
		ids[m.ID] = true
	}
	a.modelMu.Lock()
	a.upstreamModels = ids
	a.modelMu.Unlock()
}

var effortValues = append([]string{"auto"}, model.EffortLevels()...)

func effortValuesFor(id string) []string {
	m, _ := model.Find(id)
	if supported := m.Efforts; len(supported) > 0 {
		return append([]string{"auto"}, supported...)
	}
	return effortValues
}

func (a *Agent) requestedEffortLocked(s *sessionState) string {
	if s != nil && s.effort != nil {
		return *s.effort
	}
	return a.effort
}

func (a *Agent) Effort(sessionID string) (string, []string) {
	s := a.session(sessionID)
	a.modelMu.Lock()
	currentModel, _ := a.roleModelLocked(s, "")
	current := a.requestedEffortLocked(s)
	a.modelMu.Unlock()
	if current == "" {
		current = "auto"
	} else {
		m, _ := model.Find(currentModel)
		current = model.ClampEffort(current, m.Efforts)
	}
	return current, slices.Clone(effortValuesFor(currentModel))
}

func (a *Agent) effortFor(s *sessionState) string {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	requested := a.requestedEffortLocked(s)
	current, _ := a.roleModelLocked(s, "")
	m, _ := model.Find(current)
	if requested == "" {
		requested = m.Effort
	}
	return model.ClampEffort(requested, m.Efforts)
}

func (a *Agent) SetEffort(_ context.Context, sessionID, value string) error {
	if value == "auto" {
		value = ""
	} else if value != "" && !slices.Contains(effortValues, value) {
		return fmt.Errorf("effort must be auto, none, low, medium, high, xhigh, or max (got %q)", value)
	}
	s := a.session(sessionID)
	a.modelMu.Lock()
	currentModel, _ := a.roleModelLocked(s, "")
	m, _ := model.Find(currentModel)
	if supported := m.Efforts; value != "" && len(supported) > 0 && !slices.Contains(supported, value) {
		a.modelMu.Unlock()
		return fmt.Errorf("effort %q is not supported by %s (supported: %s)", value, currentModel, strings.Join(supported, ", "))
	}
	if !a.options.IsolateSessionSettings || sessionID == "" {
		a.effort = value
	}
	if s != nil {
		s.effort = &value
	}
	a.modelMu.Unlock()
	return nil
}
