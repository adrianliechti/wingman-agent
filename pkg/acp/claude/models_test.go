package claude

import (
	"context"
	"slices"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestNormalizeSessionConfig(t *testing.T) {
	models := []ModelEntry{{
		ID: "claude-sonnet", Name: "Sonnet", ResolvedModel: "claude-sonnet-4-5",
		EffortLevels: []string{"low", "high"},
	}}

	model, effort := normalizeSessionConfig(models, "Sonnet", "maximum")
	if model != "claude-sonnet" || effort != "default" {
		t.Fatalf("normalizeSessionConfig() = %q, %q", model, effort)
	}

	model, effort = normalizeSessionConfig(models, "claude-sonnet", "high")
	if model != "claude-sonnet" || effort != "high" {
		t.Fatalf("normalizeSessionConfig() = %q, %q", model, effort)
	}
}

func TestConfiguredModelDoesNotChangeGeneration(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-6", "claude-sonnet-6", "claude-sonnet-5-20260901", "claude-sonnet-5[1m]"} {
		t.Run(model, func(t *testing.T) {
			a := New(Options{Model: model})
			a.models = []ModelEntry{{ID: "sonnet", Name: "Sonnet", ResolvedModel: "claude-sonnet-5"}}
			a.modelsLoaded = true
			response, err := a.NewSession(context.Background(), acp.NewSessionRequest{Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			s := a.lookup(response.SessionId)
			args := s.cliArgsLocked()
			i := slices.Index(args, "--model")
			if i < 0 || args[i+1] != model {
				t.Fatalf("model %q produced CLI args %v", model, args)
			}
			if got := resolveResumedModel(a.models, model); got != nil {
				t.Fatalf("resumed model mapped to %+v", got)
			}
			if response.ConfigOptions[0].Select.CurrentValue != acp.SessionConfigValueId(model) {
				t.Fatalf("current model = %q", response.ConfigOptions[0].Select.CurrentValue)
			}
		})
	}
}

func TestModelPickerRejectsDifferentGeneration(t *testing.T) {
	a := New(Options{Model: "sonnet"})
	a.models = []ModelEntry{{ID: "sonnet", Name: "Sonnet", ResolvedModel: "claude-sonnet-5"}}
	a.modelsLoaded = true
	response, err := a.NewSession(context.Background(), acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
		SessionId: response.SessionId, ConfigId: modelConfigID, Value: "claude-sonnet-4-6",
	}})
	if err == nil {
		t.Fatal("picker accepted a different generation as sonnet")
	}
	if a.lookup(response.SessionId).modelID != "sonnet" {
		t.Fatal("rejected selection changed the session model")
	}
}

func TestDefaultModelDescriptionShowsResolvedModel(t *testing.T) {
	models := []ModelEntry{
		{ID: "default", Name: "Default", Description: "Recommended", ResolvedModel: "claude-sonnet-5"},
		{ID: "sonnet", Name: "Claude Sonnet 5", ResolvedModel: "claude-sonnet-5"},
	}
	option := modelConfigOption(models, "default")
	entries := *option.Select.Options.Ungrouped
	if entries[0].Description == nil || *entries[0].Description != "Claude Sonnet 5" {
		t.Fatalf("default description = %v", entries[0].Description)
	}

	models[1].ResolvedModel = "other"
	option = modelConfigOption(models, "default")
	entries = *option.Select.Options.Ungrouped
	if entries[0].Description == nil || *entries[0].Description != "claude-sonnet-5" {
		t.Fatalf("fallback description = %v", entries[0].Description)
	}
}

func TestSessionModesExposeNormalizedModes(t *testing.T) {
	state := buildSessionModeState("")
	if state.CurrentModeId != "agent" {
		t.Fatalf("current mode = %q, want agent", state.CurrentModeId)
	}
	if len(state.AvailableModes) != 3 || state.AvailableModes[0].Id != "agent" || state.AvailableModes[1].Id != "plan" || state.AvailableModes[2].Id != "unattended" {
		t.Fatalf("available modes = %#v, want agent, plan, unattended", state.AvailableModes)
	}
}

func TestModesMapToClaudePermissionModes(t *testing.T) {
	s := New(Options{}).newSession("session", "/workspace", "default", "default", nil)
	for _, test := range []struct{ mode, permission string }{
		{"agent", "auto"},
		{"plan", "plan"},
		{"unattended", "bypassPermissions"},
	} {
		s.mode = test.mode
		args := s.cliArgsLocked()
		index := slices.Index(args, "--permission-mode")
		if index < 0 || index+1 >= len(args) || args[index+1] != test.permission {
			t.Errorf("%s args = %v, want --permission-mode %s", test.mode, args, test.permission)
		}
	}
}
