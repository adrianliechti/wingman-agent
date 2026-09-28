package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/model"
)

func upstreamAgent(ids ...string) *Agent {
	models := make(map[string]bool, len(ids))
	for _, id := range ids {
		models[id] = true
	}
	return &Agent{
		upstreamModels: models,
		sessions:       map[string]*sessionState{},
	}
}

func roleModelID(t *testing.T, a *Agent, s *sessionState, role string) string {
	t.Helper()
	option, ok := a.roleModel(s, role)
	if !ok {
		t.Fatalf("role %q did not resolve", role)
	}
	return option.ID
}

func TestModelSelectionByRole(t *testing.T) {
	a := upstreamAgent("claude-sonnet-5", "claude-opus-4-8", "claude-haiku-4-5", "claude-fable-5")
	s := &sessionState{}
	for _, mode := range []sessionMode{modeAgent, modePlan} {
		s.setMode(mode)
		for _, role := range []string{"", "default", "main"} {
			if got := roleModelID(t, a, s, role); got != "claude-sonnet-5" {
				t.Fatalf("%s/%s model = %q", mode, role, got)
			}
		}
		for role, want := range map[string]string{
			"complex": "claude-opus-4-8", "plan": "claude-opus-4-8", "utility": "claude-haiku-4-5",
		} {
			if got := roleModelID(t, a, s, role); got != want {
				t.Fatalf("%s/%s model = %q, want %q", mode, role, got, want)
			}
		}
	}
}

func TestExplicitSelectionDrivesDelegationFamily(t *testing.T) {
	a := upstreamAgent("claude-sonnet-5", "claude-opus-4-8", "claude-haiku-4-5", "gpt-6-sol", "gpt-6-astra", "gpt-6-luna")
	a.modelID = "gpt-6-sol"
	s := &sessionState{}
	s.setMode(modePlan)
	for role, want := range map[string]string{
		"default": "gpt-6-sol", "complex": "gpt-6-astra", "utility": "gpt-6-luna",
	} {
		if got := roleModelID(t, a, s, role); got != want {
			t.Fatalf("%s model = %q, want %q", role, got, want)
		}
	}
	s.modelID = "claude-fable-5"
	a.upstreamModels["claude-fable-5"] = true
	if got := roleModelID(t, a, s, "complex"); got != "claude-fable-5" {
		t.Fatalf("complex replaced selected large model: %q", got)
	}
}

func TestModelSelectionCrossFamilyFallback(t *testing.T) {
	// Claude family with no small model: utility falls back to another family.
	a := upstreamAgent("claude-sonnet-5", "gpt-5.6-luna")
	if got := roleModelID(t, a, nil, "utility"); got != "gpt-5.6-luna" {
		t.Fatalf("utility model = %q, want gpt-5.6-luna", got)
	}

	// GPT-only gateway anchors every role in the gpt family.
	g := upstreamAgent("gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna")
	s := &sessionState{}
	if current := roleModelID(t, g, s, ""); current != "gpt-5.6-terra" {
		t.Fatalf("code model = %q, want gpt-5.6-terra", current)
	}
	s.setMode(modePlan)
	if current := roleModelID(t, g, s, "complex"); current != "gpt-5.6-sol" {
		t.Fatalf("complex model = %q, want gpt-5.6-sol", current)
	}
}

func TestRoleModelPreservesUtilityDiscoveryAndAvailabilityRules(t *testing.T) {
	a := &Agent{
		sessions: map[string]*sessionState{},
	}
	if _, ok := a.RoleModel("utility"); ok {
		t.Fatal("utility role guessed a model before discovery")
	}
	if got := roleModelID(t, a, nil, ""); got != "claude-sonnet-5-5" {
		t.Fatalf("main model before discovery = %q", got)
	}

	a.utilityModel = "configured-utility"
	if got := roleModelID(t, a, nil, "utility"); got != "configured-utility" {
		t.Fatalf("configured utility model = %q", got)
	}
	a.modelID = "qwen3.8:27b-mlx"
	if got := roleModelID(t, a, nil, ""); got != "qwen3.8:27b-mlx" {
		t.Fatalf("configured main model before discovery = %q", got)
	}

	a.upstreamModels = map[string]bool{"gpt-5.6-luna": true}
	a.modelID = "missing-main"
	if got := roleModelID(t, a, nil, ""); got != "gpt-5.6-luna" {
		t.Fatalf("main availability fallback = %q", got)
	}
	if got := roleModelID(t, a, nil, "utility"); got != "configured-utility" {
		t.Fatalf("utility availability unexpectedly replaced %q", got)
	}
}

func TestModelSelectionAcrossProviderPrefixes(t *testing.T) {
	a := upstreamAgent("gpt-6-astra", "google/gemini-3.7-flash", "gemini-3.1-pro")
	if got := roleModelID(t, a, nil, "main"); got != "google/gemini-3.7-flash" {
		t.Fatalf("main model = %q", got)
	}
	if got := roleModelID(t, a, nil, "complex"); got != "gemini-3.1-pro" {
		t.Fatalf("complex model = %q, want the selected model's family", got)
	}
}

func TestModelSelectionPreservesConfiguredAliases(t *testing.T) {
	for _, tc := range []struct {
		configured string
		advertised []string
		want       string
	}{
		{"gpt-5.6", []string{"openai/gpt-5.6-sol"}, "openai/gpt-5.6-sol"},
		{"GPT-6-SOL", []string{"openai/gpt-6-sol"}, "openai/gpt-6-sol"},
		{"gpt-6-sol", []string{"gpt-6-sol", "openai/gpt-6-sol"}, "gpt-6-sol"},
		{"custom-deployment", []string{"custom-deployment"}, "custom-deployment"},
	} {
		t.Run(tc.configured, func(t *testing.T) {
			a := upstreamAgent(append([]string{"claude-sonnet-5-5"}, tc.advertised...)...)
			a.modelID = tc.configured
			if got := roleModelID(t, a, nil, "main"); got != tc.want {
				t.Fatalf("configured %q resolved to %q, want %q", tc.configured, got, tc.want)
			}
			options, current := a.Models("")
			if current != tc.want || !slices.ContainsFunc(options, func(m model.Model) bool { return m.ID == current }) {
				t.Fatalf("picker lost selected model %q: current=%q options=%+v", tc.want, current, options)
			}
		})
	}
}

func TestSessionAutoEffortOverridesGlobalSetting(t *testing.T) {
	for _, action := range []string{"auto", "switch-model"} {
		t.Run(action, func(t *testing.T) {
			a := upstreamAgent("gpt-6-astra", "gpt-6-sol")
			a.options.IsolateSessionSettings = true
			a.modelID = "gpt-6-astra"
			a.effort = "max"
			s := &sessionState{}
			a.sessions["one"] = s
			a.sessions["two"] = &sessionState{}
			wantEffort := "low"
			var err error
			if action == "auto" {
				err = a.SetEffort(context.Background(), "one", "auto")
			} else {
				err = a.SetModel(context.Background(), "one", "gpt-6-sol")
				wantEffort = ""
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := a.Effort("one"); got != "auto" {
				t.Errorf("session effort selector = %q, want auto", got)
			}
			if got := a.effortFor(s); got != wantEffort {
				t.Errorf("effective session effort = %q, want %q", got, wantEffort)
			}
			for _, sid := range []string{"two", ""} {
				if got, _ := a.Effort(sid); got != "max" {
					t.Errorf("unrelated session %q effort = %q, want max", sid, got)
				}
			}
		})
	}
}

func TestEffortDefaultsFollowModelAcrossModes(t *testing.T) {
	for _, id := range []string{"claude-sonnet-5-5", "gpt-6-sol", "gpt-6-astra", "qwen3.8:27b-mlx"} {
		t.Run(id, func(t *testing.T) {
			a := upstreamAgent(id)
			s := &sessionState{}
			m, _ := model.Find(id)
			for _, mode := range []sessionMode{modeAgent, modePlan} {
				s.setMode(mode)
				if got := a.effortFor(s); got != m.Effort {
					t.Fatalf("%s auto effort = %q, want model default %q", mode, got, m.Effort)
				}
			}
			effort := "medium"
			s.effort = &effort
			for _, mode := range []sessionMode{modePlan, modeAgent} {
				s.setMode(mode)
				if got := a.effortFor(s); got != effort {
					t.Fatalf("%s lost explicit effort: %q", mode, got)
				}
			}
		})
	}
}

func TestAstraEffortDefaultsAndClampsOverrides(t *testing.T) {
	a := upstreamAgent("gpt-6-astra")
	s := &sessionState{}

	if got := a.effortFor(s); got != "low" {
		t.Fatalf("Astra effort = %q, want low", got)
	}

	a.effort = "none"
	if got := a.effortFor(s); got != "low" {
		t.Fatalf("Astra effort for unsupported none override = %q, want low", got)
	}
	if current, values := a.Effort(""); current != "low" || !slices.Equal(values, []string{"auto", "low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("Astra effort selector = %q/%v", current, values)
	}

	if err := a.SetEffort(context.Background(), "", "none"); err == nil {
		t.Fatal("SetEffort accepted unsupported Astra effort none")
	}
}

func TestQwen38EffortDefaultsAndClampsOverrides(t *testing.T) {
	a := upstreamAgent("qwen3.8:27b-mlx")
	s := &sessionState{}

	if got := a.effortFor(s); got != "" {
		t.Fatalf("Qwen 3.8 auto effort = %q, want provider default", got)
	}

	a.effort = "max"
	if got := a.effortFor(s); got != "xhigh" {
		t.Fatalf("Qwen 3.8 max effort = %q, want xhigh", got)
	}
	if current, values := a.Effort(""); current != "xhigh" || !slices.Equal(values, []string{"auto", "none", "low", "medium", "xhigh"}) {
		t.Fatalf("Qwen 3.8 effort selector = %q/%v", current, values)
	}

	if err := a.SetEffort(context.Background(), "", "high"); err == nil {
		t.Fatal("SetEffort accepted unsupported Qwen 3.8 effort high")
	}
}

func TestModelAndEffortSelectionSurviveModeSwitches(t *testing.T) {
	a := upstreamAgent("claude-sonnet-5", "claude-opus-4-8", "claude-fable-5")
	s := &sessionState{}
	a.sessions["sid"] = s
	ctx := context.Background()
	for _, mode := range []string{"agent", "plan", "agent"} {
		if err := a.SetMode(ctx, "sid", mode); err != nil {
			t.Fatal(err)
		}
		if err := a.SetModel(ctx, "sid", "claude-fable-5"); err != nil {
			t.Fatal(err)
		}
		if err := a.SetEffort(ctx, "sid", "max"); err != nil {
			t.Fatal(err)
		}
		for _, next := range []string{"agent", "plan"} {
			if err := a.SetMode(ctx, "sid", next); err != nil {
				t.Fatal(err)
			}
			if _, got := a.Models("sid"); got != "claude-fable-5" {
				t.Fatalf("model after %s -> %s = %q", mode, next, got)
			}
			if got, _ := a.Effort("sid"); got != "max" || a.effortFor(s) != "max" {
				t.Fatalf("effort after %s -> %s = %q", mode, next, got)
			}
		}
	}
}

func TestSetModelResetsEffort(t *testing.T) {
	a := upstreamAgent("claude-sonnet-5", "gpt-5.6-terra", "claude-opus-4-8")
	s := &sessionState{}
	a.sessions["sid"] = s
	ctx := context.Background()

	if err := a.SetEffort(ctx, "sid", "max"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Effort("sid"); got != "max" {
		t.Fatalf("effort = %q, want max", got)
	}

	if err := a.SetModel(ctx, "sid", "gpt-5.6-terra"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Effort("sid"); got != "auto" {
		t.Fatalf("effort after model switch = %q, want auto (model default)", got)
	}

	// A deliberate model change in Plan also resets the shared effort to auto.
	s.setMode(modePlan)
	if err := a.SetEffort(ctx, "sid", "low"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetModel(ctx, "sid", "claude-opus-4-8"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Effort("sid"); got != "auto" {
		t.Fatalf("plan effort after switch = %q, want auto", got)
	}
	if got := a.effortFor(s); got != "" {
		t.Fatalf("auto effort = %q, want provider default", got)
	}
}

func TestSelectingCurrentModelPreservesEffort(t *testing.T) {
	for _, isolate := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "isolated"}[isolate], func(t *testing.T) {
			a := upstreamAgent("openai/gpt-6-sol", "gpt-6-astra")
			a.options.IsolateSessionSettings = isolate
			a.modelID, a.effort = "gpt-6-sol", "high"
			s := &sessionState{}
			a.sessions["sid"] = s
			for _, id := range []string{"gpt-6-sol", "openai/gpt-6-sol", "GPT-6-SOL"} {
				if err := a.SetModel(t.Context(), "sid", id); err != nil {
					t.Fatal(err)
				}
				if got, _ := a.Effort("sid"); got != "high" {
					t.Fatalf("reselecting %s changed effort to %s", id, got)
				}
			}
		})
	}
}

func TestSelectingSessionModelPreservesInheritedEffortWhileUpdatingDefaults(t *testing.T) {
	a := upstreamAgent("gpt-6-sol", "gpt-6-astra")
	a.modelID, a.effort = "gpt-6-astra", "high"
	a.sessions["sid"] = &sessionState{modelID: "gpt-6-sol"}
	if err := a.SetModel(t.Context(), "sid", "gpt-6-sol"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Effort("sid"); got != "high" {
		t.Fatalf("same session model lost inherited effort: %s", got)
	}
	if got, _ := a.Effort(""); got != "auto" {
		t.Fatalf("changed default model kept previous model's effort: %s", got)
	}
}

func TestComplexWaitsForModelDiscovery(t *testing.T) {
	a := &Agent{modelID: "local-deployment"}
	if got := roleModelID(t, a, nil, "complex"); got != "local-deployment" {
		t.Fatalf("undiscovered complex role guessed unavailable model %q", got)
	}
	a.complexModel = "explicit-complex"
	if got := roleModelID(t, a, nil, "complex"); got != "explicit-complex" {
		t.Fatalf("explicit complex model lost before discovery: %q", got)
	}
	a.complexModel = ""
	a.upstreamModels = map[string]bool{"local-deployment": true, "gpt-6-astra": true}
	if got := roleModelID(t, a, nil, "complex"); got != "gpt-6-astra" {
		t.Fatalf("discovered complex model = %q", got)
	}
}

func TestModelOptionsAdvertiseSupportedEfforts(t *testing.T) {
	a := upstreamAgent("openai/gpt-6-astra", "claude-sonnet-5-5", "qwen3.8:27b-mlx", "local-deployment")
	a.modelID = "local-deployment"
	options, _ := a.Models("")
	for _, option := range options {
		if err := a.SetModel(t.Context(), "", option.ID); err != nil {
			t.Fatal(err)
		}
		_, supported := a.Effort("")
		if !slices.Equal(supported, append([]string{"auto"}, option.Efforts...)) {
			t.Fatalf("%s advertised %v, picker supports %v", option.ID, option.Efforts, supported)
		}
		for _, effort := range option.Efforts {
			if err := a.SetEffort(t.Context(), "", effort); err != nil {
				t.Fatalf("%s rejected advertised effort %s: %v", option.ID, effort, err)
			}
		}
	}
}

func TestModelClass(t *testing.T) {
	tests := map[string]model.Class{
		"gpt-6-astra":       model.ClassLarge,
		"claude-opus-5-5":   model.ClassLarge,
		"claude-opus-5":     model.ClassLarge,
		"claude-opus-4-8":   model.ClassLarge,
		"gpt-5.6-sol":       model.ClassLarge,
		"claude-fable-5":    model.ClassLarge,
		"claude-sonnet-5-5": model.ClassMedium,
		"claude-sonnet-5":   model.ClassMedium,
		"gpt-5.6-terra":     model.ClassMedium,
		"gpt-5.3-codex":     model.ClassMedium,
		"claude-haiku-4-5":  model.ClassSmall,
		"gpt-5.6-luna":      model.ClassSmall,
		"deepseek-v4-flash": model.ClassSmall,
	}
	for id, want := range tests {
		if got := model.ClassOf(id); got != want {
			t.Errorf("ModelClassOf(%q) = %d, want %d", id, got, want)
		}
	}

	if model.Family("claude-sonnet-5") != "claude" || model.Family("gpt-5.6-sol") != "gpt" {
		t.Fatal("ModelFamilyOf broken")
	}
}

func TestExplicitDelegationModelsDoNotChangeSessionSelection(t *testing.T) {
	a := upstreamAgent("claude-sonnet-5", "claude-opus-4-8", "claude-haiku-4-5", "claude-fable-5")
	a.complexModel = "claude-fable-5"
	a.utilityModel = "claude-sonnet-5"
	s := &sessionState{modelID: "claude-opus-4-8"}
	for _, mode := range []sessionMode{modeAgent, modePlan} {
		s.setMode(mode)
		for role, want := range map[string]string{
			"default": "claude-opus-4-8", "complex": "claude-fable-5", "plan": "claude-fable-5", "utility": "claude-sonnet-5",
		} {
			if got := roleModelID(t, a, s, role); got != want {
				t.Fatalf("%s/%s model = %q, want %q", mode, role, got, want)
			}
		}
	}
}

func TestUnavailableDelegationTierKeepsSelectedModel(t *testing.T) {
	a := upstreamAgent("claude-sonnet-5", "gpt-6-sol")
	a.modelID = "gpt-6-sol"
	for _, role := range []string{"complex", "utility"} {
		if got := roleModelID(t, a, nil, role); got != "gpt-6-sol" {
			t.Fatalf("unavailable %s tier replaced selected model with %q", role, got)
		}
	}
	a.complexModel = "missing-model"
	if got := roleModelID(t, a, nil, "complex"); got != "gpt-6-sol" {
		t.Fatalf("unavailable override replaced selected model with %q", got)
	}
}

func TestWebSessionSettingsDoNotChangeOtherSessionsOrDefaults(t *testing.T) {
	a := upstreamAgent("gpt-5.4", "gpt-5.5")
	a.options.IsolateSessionSettings = true
	a.modelID = "gpt-5.4"
	a.sessions["one"] = &sessionState{}
	a.sessions["two"] = &sessionState{}
	if err := a.SetModel(context.Background(), "one", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetEffort(context.Background(), "one", "high"); err != nil {
		t.Fatal(err)
	}
	if _, got := a.Models("one"); got != "gpt-5.5" {
		t.Fatalf("first model: %s", got)
	}
	for _, sid := range []string{"two", ""} {
		if _, got := a.Models(sid); got != "gpt-5.4" {
			t.Fatalf("%q model changed: %s", sid, got)
		}
		if got, _ := a.Effort(sid); got == "high" {
			t.Fatalf("%q effort changed", sid)
		}
	}
}
