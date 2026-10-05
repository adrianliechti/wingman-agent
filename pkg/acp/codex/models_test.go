package codex

import "testing"

func TestNormalizeSessionConfig(t *testing.T) {
	models := []modelEntry{
		{ID: "gpt-default", Default: true, EffortLevels: []string{"low", "high"}},
		{ID: "gpt-other", EffortLevels: []string{"medium"}},
	}

	model, effort := normalizeSessionConfig(models, "default", "maximum")
	if model != "gpt-default" || effort != "default" {
		t.Fatalf("normalizeSessionConfig() = %q, %q", model, effort)
	}

	model, effort = normalizeSessionConfig(models, "gpt-other", "medium")
	if model != "gpt-other" || effort != "medium" {
		t.Fatalf("normalizeSessionConfig() = %q, %q", model, effort)
	}
}

func TestSessionModesExposeNormalizedPolicies(t *testing.T) {
	state := buildSessionModeState("")
	if state.CurrentModeId != "agent" {
		t.Fatalf("current mode = %q, want agent", state.CurrentModeId)
	}
	if len(state.AvailableModes) != 3 || state.AvailableModes[0].Id != "agent" || state.AvailableModes[1].Id != "plan" || state.AvailableModes[2].Id != "unattended" {
		t.Fatalf("available modes = %#v, want agent, plan, unattended", state.AvailableModes)
	}
	if mode := modeFor("plan"); mode.approvalPolicy != "on-request" || mode.sandboxPolicy.(map[string]any)["type"] != "readOnly" {
		t.Fatalf("plan policy = %#v", mode)
	}
	if mode := modeFor("unattended"); mode.approvalPolicy != "never" || mode.sandboxPolicy.(map[string]any)["type"] != "dangerFullAccess" {
		t.Fatalf("unattended policy = %#v", mode)
	}
}

func TestFormatModelDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"gpt-6-astra": "6 Astra", "GPT-5.6-Sol": "5.6 Sol", "gpt-5.5": "5.5",
		"GPT-6.1-Sol":         "6.1 Sol",
		"gpt-5.3-codex-spark": "5.3 Codex Spark", "gpt-5.3/codex-spark": "5.3 Codex Spark", "gpt-oss-120B": "Oss 120B",
		"Claude Opus": "Claude Opus", "custom-provider/model-v2": "custom-provider/model-v2", "o3-mini": "o3-mini",
	} {
		if got := formatModelDisplayName(in); got != want {
			t.Errorf("formatModelDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
	models := modelsFromCodex([]codexModel{{ID: "gpt-6-astra", DisplayName: "GPT-6-Astra"}, {ID: "custom", DisplayName: ""}})
	if models[0].ID != "gpt-6-astra" || models[0].Name != "6 Astra" || models[1].Name != "custom" {
		t.Fatalf("models = %#v", models)
	}
	option := modelConfigOption(models, "gpt-5.5-uncataloged")
	if first := (*option.Select.Options.Ungrouped)[0]; first.Value != "gpt-5.5-uncataloged" || first.Name != "5.5 Uncataloged" {
		t.Fatalf("uncataloged option = %#v", first)
	}
}
