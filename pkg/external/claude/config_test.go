package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			if *cfg != want {
				t.Fatalf("NewConfig() = %+v, want %+v", *cfg, want)
			}

			// Both the launcher and the Wingman ACP backend use BuildEnv.
			env := BuildEnv([]string{"ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-4-5"}, cfg)
			for key, value := range map[string]string{
				"ANTHROPIC_DEFAULT_HAIKU_MODEL":  want.HaikuModel,
				"ANTHROPIC_DEFAULT_SONNET_MODEL": want.SonnetModel,
				"ANTHROPIC_DEFAULT_OPUS_MODEL":   want.OpusModel,
				"ANTHROPIC_DEFAULT_FABLE_MODEL":  want.FableModel,
				"CLAUDE_CODE_SUBAGENT_MODEL":     want.SonnetModel,
			} {
				if !slices.Contains(env, key+"="+value) {
					t.Errorf("launch environment missing %s=%s", key, value)
				}
			}
		})
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
