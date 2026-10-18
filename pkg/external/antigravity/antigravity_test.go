package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/model"
)

func TestConfigDirUsesWingmanHome(t *testing.T) {
	home := testenv.WingmanHome(t)

	got, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "antigravity"); got != want {
		t.Fatalf("ConfigDir() = %q, want %q", got, want)
	}
}

func TestBuildArgsSelectsDefaultModel(t *testing.T) {
	cfg := &AntigravityConfig{Model: "gemini-3.8-flash"}

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{nil, []string{"--gemini_dir", "/d", "--app_data_dir", "antigravity-cli", "--model", "gemini-3.8-flash"}},
		{[]string{"-p", "hi"}, []string{"--gemini_dir", "/d", "--app_data_dir", "antigravity-cli", "--model", "gemini-3.8-flash", "-p", "hi"}},
		{[]string{"--model=claude-sonnet-5-5"}, []string{"--gemini_dir", "/d", "--app_data_dir", "antigravity-cli", "--model=claude-sonnet-5-5"}},
		{[]string{"mcp", "list"}, []string{"--gemini_dir", "/d", "--app_data_dir", "antigravity-cli", "mcp", "list"}},
	} {
		if got := BuildArgs("/d", cfg, tc.args); !slices.Equal(got, tc.want) {
			t.Errorf("BuildArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestBuildEnvReplacesInheritedCredentials(t *testing.T) {
	cfg := &AntigravityConfig{BaseURL: "http://gateway", AuthToken: "token"}
	env := BuildEnv([]string{"GEMINI_API_KEY=user", "AGY_ADC_AUTH=true", "PATH=/bin"}, cfg)

	for _, want := range []string{"PATH=/bin", "GEMINI_API_KEY=token", "GOOGLE_GEMINI_BASE_URL=http://gateway", "AGY_CLI_DISABLE_AUTO_UPDATE=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("BuildEnv() = %q, missing %q", env, want)
		}
	}
	for _, unwanted := range []string{"GEMINI_API_KEY=user", "AGY_ADC_AUTH=true"} {
		if slices.Contains(env, unwanted) {
			t.Errorf("BuildEnv() = %q, kept %q", env, unwanted)
		}
	}
}

func TestWriteSettingsKeepsAntigravityState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "antigravity-cli", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"trustedWorkspaces":["/src"],"modelProvider":"other","customModelsConfig":{"customModels":{"stale":{}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &AntigravityConfig{Models: []model.Model{{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", Context: 1000000}}}
	if err := WriteSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		TrustedWorkspaces  []string `json:"trustedWorkspaces"`
		ModelProvider      string   `json:"modelProvider"`
		EnableTelemetry    *bool    `json:"enableTelemetry"`
		CustomModelsConfig struct {
			CustomModels map[string]struct {
				ModelID       string         `json:"modelId"`
				ModelName     string         `json:"modelName"`
				APIProvider   string         `json:"apiProvider"`
				ModelFeatures map[string]any `json:"modelFeatures"`
			} `json:"customModels"`
		} `json:"customModelsConfig"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(got.TrustedWorkspaces, []string{"/src"}) {
		t.Errorf("trustedWorkspaces = %q, want it kept", got.TrustedWorkspaces)
	}
	if got.ModelProvider != "gemini" || got.EnableTelemetry == nil || *got.EnableTelemetry {
		t.Errorf("modelProvider = %q, enableTelemetry = %v", got.ModelProvider, got.EnableTelemetry)
	}
	models := got.CustomModelsConfig.CustomModels
	if _, ok := models["stale"]; ok || len(models) != 1 {
		t.Fatalf("customModels = %v, want only the gateway model", models)
	}
	m := models["claude-sonnet-5-5"]
	if m.ModelID != "claude-sonnet-5-5" || m.ModelName != "gemini-api://claude-sonnet-5-5" || m.APIProvider != "API_PROVIDER_GOOGLE_GEMINI" {
		t.Errorf("customModels[claude-sonnet-5-5] = %+v", m)
	}
	if m.ModelFeatures["supportsImages"] != true {
		t.Errorf("modelFeatures = %v, want images for Claude", m.ModelFeatures)
	}
}

func TestDefaultModelPrefersGemini(t *testing.T) {
	models := []model.Model{
		{ID: "claude-sonnet-5-5"},
		{ID: "gemini-3.8-flash-lite", Class: model.ClassSmall},
		{ID: "gemini-3.8-flash"},
	}
	if got := defaultModel(models); got != "gemini-3.8-flash" {
		t.Fatalf("defaultModel() = %q, want gemini-3.8-flash", got)
	}
	if got := defaultModel(models[:1]); got != "claude-sonnet-5-5" {
		t.Fatalf("defaultModel() = %q, want claude-sonnet-5-5", got)
	}
}
