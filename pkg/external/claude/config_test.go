package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConfigPrefersLatestAvailableModels(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
		want ClaudeConfig
	}{
		{
			name: "latest alongside older models",
			ids: []string{
				"claude-haiku-4-5", "claude-sonnet-5", "claude-opus-5", "claude-fable-5",
				"claude-haiku-4-6", "anthropic/claude-haiku-5-5",
				"anthropic/claude-sonnet-5-5", "anthropic/claude-opus-5-5", "anthropic/claude-fable-5-1",
			},
			want: ClaudeConfig{
				HaikuModel: "anthropic/claude-haiku-5-5", SonnetModel: "anthropic/claude-sonnet-5-5",
				OpusModel: "anthropic/claude-opus-5-5", FableModel: "anthropic/claude-fable-5-1",
			},
		},
		{
			name: "older backend fallback",
			ids:  []string{"claude-haiku-4-5", "claude-sonnet-5", "claude-opus-5", "claude-fable-5"},
			want: ClaudeConfig{
				HaikuModel: "claude-haiku-4-5", SonnetModel: "claude-sonnet-5",
				OpusModel: "claude-opus-5", FableModel: "claude-fable-5",
			},
		},
		{
			name: "no fable or haiku",
			ids:  []string{"claude-sonnet-4-6"},
			want: ClaudeConfig{SonnetModel: "claude-sonnet-4-6"},
		},
		{
			name: "opus only",
			ids:  []string{"claude-opus-4-6"},
			want: ClaudeConfig{OpusModel: "claude-opus-4-6", DefaultModel: "claude-opus-4-6"},
		},
		{
			name: "haiku only",
			ids:  []string{"claude-haiku-4-5"},
			want: ClaudeConfig{HaikuModel: "claude-haiku-4-5", DefaultModel: "claude-haiku-4-5"},
		},
		{
			name: "new model outside the catalog",
			ids:  []string{"internal/claude-future-99"},
			want: ClaudeConfig{DefaultModel: "internal/claude-future-99"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/v1/models" {
					t.Errorf("unexpected path %q", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				data := make([]map[string]string, 0, len(tc.ids))
				for _, id := range tc.ids {
					data = append(data, map[string]string{"id": id, "object": "model"})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			}))
			defer server.Close()

			cfg, err := NewConfig(context.Background(), &Options{WingmanURL: server.URL, WingmanToken: "test-token"})
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			want.BaseURL, want.AuthToken = server.URL, "test-token"
			if want.DefaultModel == "" {
				want.DefaultModel = want.SonnetModel
			}
			if cfg.BaseURL != want.BaseURL || cfg.AuthToken != want.AuthToken ||
				cfg.DefaultModel != want.DefaultModel || cfg.HaikuModel != want.HaikuModel ||
				cfg.SonnetModel != want.SonnetModel || cfg.OpusModel != want.OpusModel || cfg.FableModel != want.FableModel {
				t.Fatalf("NewConfig() = %+v, want %+v", *cfg, want)
			}
			gotIDs, wantIDs := slices.Clone(cfg.Models), slices.Clone(tc.ids)
			slices.Sort(gotIDs)
			slices.Sort(wantIDs)
			if !slices.Equal(gotIDs, wantIDs) {
				t.Fatalf("available models = %v, want %v", gotIDs, wantIDs)
			}
			if requests.Load() != 1 {
				t.Fatalf("model list fetched %d times, want once", requests.Load())
			}

			// Both the launcher and the Wingman ACP backend use BuildEnv.
			env := BuildEnv([]string{"ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-4-5"}, cfg)
			haiku := want.HaikuModel
			if haiku == "" {
				haiku = want.DefaultModel
			}
			for key, value := range map[string]string{
				"ANTHROPIC_DEFAULT_HAIKU_MODEL":  haiku,
				"ANTHROPIC_DEFAULT_SONNET_MODEL": want.SonnetModel,
				"ANTHROPIC_DEFAULT_OPUS_MODEL":   want.OpusModel,
				"ANTHROPIC_DEFAULT_FABLE_MODEL":  want.FableModel,
				"CLAUDE_CODE_SUBAGENT_MODEL":     "inherit",
				"ANTHROPIC_MODEL":                want.DefaultModel,
			} {
				if !slices.Contains(env, key+"="+value) {
					t.Errorf("launch environment missing %s=%s", key, value)
				}
			}
		})
	}
}

func TestConfigRejectsGatewayWithoutClaudeModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-6-astra"}]}`)
	}))
	defer server.Close()
	_, err := NewConfig(context.Background(), &Options{WingmanURL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "no available Claude models") {
		t.Fatalf("NewConfig() error = %v, want no Claude models", err)
	}
}

func TestBuildArgsRestrictsModelsThroughParentPolicy(t *testing.T) {
	cfg := &ClaudeConfig{Models: []string{"claude-sonnet-5", "anthropic/claude-haiku-4-5"}}
	args := BuildArgs(cfg)
	i := slices.Index(args, "--managed-settings")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("missing parent policy in %v", args)
	}
	var policy struct {
		Models  []string `json:"availableModels"`
		Match   string   `json:"availableModelsMatch"`
		Enforce bool     `json:"enforceAvailableModels"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &policy); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(policy.Models, cfg.Models) || policy.Match != "exact" || !policy.Enforce {
		t.Fatalf("incorrect model policy: %+v", policy)
	}
	if slices.Contains(BuildArgs(nil), "--managed-settings") {
		t.Fatal("native backend must not receive a gateway model policy")
	}
}

func TestBuildEnvOverridesInheritedModelsAndProviders(t *testing.T) {
	cfg := &ClaudeConfig{DefaultModel: "claude-sonnet-5", SonnetModel: "claude-sonnet-5"}
	parent := []string{
		"PATH=/usr/bin", "ANTHROPIC_MODEL=claude-fable-5",
		"ANTHROPIC_DEFAULT_FABLE_MODEL=claude-fable-5", "CLAUDE_CODE_SUBAGENT_MODEL=claude-fable-5",
		"CLAUDE_CODE_SUBAGENT_MODEL_FORCE=1",
		"ANTHROPIC_SMALL_FAST_MODEL=claude-haiku-3-5",
		"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=0",
	}
	env := BuildEnv(parent, cfg)
	for _, entry := range parent[1:] {
		if slices.Contains(env, entry) {
			t.Errorf("stale environment entry %q survived", entry)
		}
	}
	for _, want := range []string{
		"PATH=/usr/bin", "ANTHROPIC_MODEL=claude-sonnet-5", "ANTHROPIC_DEFAULT_FABLE_MODEL=",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-sonnet-5", "CLAUDE_CODE_SUBAGENT_MODEL=inherit",
		"CLAUDE_CODE_SUBAGENT_MODEL_FORCE=0",
		"CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("launch environment missing %q", want)
		}
	}
	seen := make(map[string]bool)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key == "ANTHROPIC_SMALL_FAST_MODEL" {
			t.Error("deprecated small/fast model override should be absent")
		}
		if seen[key] {
			t.Errorf("duplicate environment key %q", key)
		}
		seen[key] = true
	}
}

func TestConfigPreservesProviderIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"anthropic/CLAUDE-SONNET-5-5","object":"model"},
			{"id":"anthropic/claude-opus-5-5","object":"model"},
			{"id":"claude-haiku-4-5:latest","object":"model"},
			{"id":"~anthropic/claude-fable-5-1","object":"model"},
			{"id":"gpt-6-astra","object":"model"}
		]}`)
	}))
	defer server.Close()
	cfg, err := NewConfig(context.Background(), &Options{WingmanURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SonnetModel != "anthropic/CLAUDE-SONNET-5-5" || cfg.OpusModel != "anthropic/claude-opus-5-5" || cfg.HaikuModel != "claude-haiku-4-5:latest" || cfg.FableModel != "~anthropic/claude-fable-5-1" {
		t.Fatalf("config did not preserve provider IDs: %+v", cfg)
	}
}
