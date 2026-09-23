package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestBuildModelCatalogFiltersModelsAndResolvesAliases(t *testing.T) {
	data, err := buildModelCatalog([]string{
		"gpt-6-astra",
		"gpt-6-sol",
		"gpt-6-sol",
		"gpt-6-luna",
		"gpt-5.6-terra",
		"gpt-5.4",
		"gpt-5.3-codex",
		"gpt-5.3-codex",
		"gpt-unknown",
		"gpt-5.6",
		"claude-sonnet-5",
	})
	if err != nil {
		t.Fatal(err)
	}

	var catalog struct {
		Models []struct {
			Slug              string `json:"slug"`
			DisplayName       string `json:"display_name"`
			Description       string `json:"description"`
			Visibility        string `json:"visibility"`
			Priority          int    `json:"priority"`
			ContextWindow     int    `json:"context_window"`
			DefaultEffort     string `json:"default_reasoning_level"`
			ShellType         string `json:"shell_type"`
			MultiAgentVersion string `json:"multi_agent_version"`
			ModelMessages     *struct {
				Instructions string `json:"instructions_template"`
			} `json:"model_messages"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}

	gotIDs := make([]string, 0, len(catalog.Models))
	for i, entry := range catalog.Models {
		gotIDs = append(gotIDs, entry.Slug)
		if entry.Visibility != "list" {
			t.Errorf("model %q visibility = %q, want list", entry.Slug, entry.Visibility)
		}
		if entry.Priority != i+1 {
			t.Errorf("model %q priority = %d, want %d", entry.Slug, entry.Priority, i+1)
		}
		if entry.ModelMessages == nil || entry.ModelMessages.Instructions == "" {
			t.Errorf("model %q has no embedded instructions", entry.Slug)
		}
		if entry.MultiAgentVersion != "v1" {
			t.Errorf("model %q multi-agent version = %q, want v1", entry.Slug, entry.MultiAgentVersion)
		}
	}

	if want := []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.4", "gpt-5.6"}; !slices.Equal(gotIDs, want) {
		t.Fatalf("model ids = %q, want %q", gotIDs, want)
	}

	astra := catalog.Models[0]
	if astra.DefaultEffort != "low" || astra.ShellType != "shell_command" {
		t.Errorf("Astra catalog metadata = effort %q, shell %q", astra.DefaultEffort, astra.ShellType)
	}
	if !strings.Contains(astra.ModelMessages.Instructions, "You are Codex, an agent based on GPT-6") {
		t.Error("Astra catalog is missing its GPT-6 instructions")
	}
	for _, entry := range catalog.Models[1:3] {
		if entry.DefaultEffort != "medium" || entry.ShellType != "shell_command" || entry.ContextWindow != 272_000 {
			t.Errorf("%s catalog metadata = effort %q, shell %q, context %d", entry.Slug, entry.DefaultEffort, entry.ShellType, entry.ContextWindow)
		}
		if !strings.Contains(entry.ModelMessages.Instructions, "You are Codex, an agent based on GPT-6") {
			t.Errorf("%s is missing GPT-6 instructions", entry.Slug)
		}
	}

	alias := catalog.Models[5]
	if alias.DisplayName != "GPT 5.6 Sol" || alias.ContextWindow != 272_000 {
		t.Errorf("alias metadata = name %q, context %d", alias.DisplayName, alias.ContextWindow)
	}
}

func TestCatalogPreservesUpstreamConfiguration(t *testing.T) {
	var upstream modelCatalog
	if err := json.Unmarshal(embeddedModelCatalog, &upstream); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6"} {
		t.Run(id, func(t *testing.T) {
			upstreamID := id
			if id == "gpt-5.6" {
				upstreamID = "gpt-5.6-sol"
			}
			var original map[string]any
			for _, entry := range upstream.Models {
				if entry["slug"] == upstreamID {
					original = entry
				}
			}
			if original == nil {
				t.Fatal("missing dedicated upstream entry")
			}
			data, err := buildModelCatalog([]string{id})
			if err != nil {
				t.Fatal(err)
			}
			var catalog modelCatalog
			if err := json.Unmarshal(data, &catalog); err != nil {
				t.Fatal(err)
			}
			for key, want := range original {
				switch key {
				case "slug", "display_name", "visibility", "priority", "availability_nux", "upgrade", "multi_agent_version":
					continue // Intentional Wingman runtime overrides.
				}
				if got := catalog.Models[0][key]; !reflect.DeepEqual(got, want) {
					t.Errorf("upstream field %q was changed", key)
				}
			}
		})
	}
}

func TestPrepareModelCatalogCleansUp(t *testing.T) {
	cfg := &CodexConfig{Models: []string{"gpt-5.4"}}
	cleanup, err := PrepareModelCatalog(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.ModelCatalog

	if !filepath.IsAbs(path) {
		t.Fatalf("catalog path = %q, want absolute", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("catalog permissions = %o, want private", info.Mode().Perm())
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("catalog file still exists after cleanup: %v", err)
	}
}

func TestBuildModelCatalogRequiresMatchingModel(t *testing.T) {
	for _, ids := range [][]string{nil, {"claude-sonnet-5"}, {"gpt-5.2", "gpt-unknown"}} {
		_, err := buildModelCatalog(ids)
		if err == nil || !strings.Contains(err.Error(), "no available OpenAI models match") {
			t.Errorf("buildModelCatalog(%q) error = %v, want no matching models", ids, err)
		}
	}
}
