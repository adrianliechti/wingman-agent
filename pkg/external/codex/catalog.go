package codex

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/model"
)

// models.json contains unmodified entries from codex-rs/models-manager/models.json.
// Keeping the full upstream entries preserves Codex's model-specific
// instructions and tool metadata. Run `task generate:codex-models` to refresh
// it; Wingman-specific fields are changed only in the temporary runtime copy.
//
//go:embed models.json
var embeddedModelCatalog []byte

// Retain the last upstream entries for supported models removed from Codex's
// current catalog. Refreshing models.json must still allow these deployments.
//
//go:embed models_legacy.json
var legacyModelCatalog []byte

type modelCatalog struct {
	Models []map[string]any `json:"models"`
}

// PrepareModelCatalog writes the Wingman-filtered catalog to a temporary file.
// The caller must keep the file alive while Codex runs and call cleanup after it exits.
func PrepareModelCatalog(cfg *CodexConfig) (cleanup func(), err error) {
	data, err := buildModelCatalog(cfg.Models)
	if err != nil {
		return nil, err
	}

	file, err := os.CreateTemp("", "wingman-codex-models-*.json")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	cleanup = func() { _ = os.Remove(path) }

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		cleanup()
		return nil, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, err
	}

	cfg.ModelCatalog = path
	return cleanup, nil
}

// Launches share the immutable upstream templates. Each runtime catalog
// applies its overrides to a separate copy of the selected entries.
var modelTemplates = sync.OnceValues(func() (map[string]map[string]any, error) {
	templates := make(map[string]map[string]any)
	for _, data := range [][]byte{legacyModelCatalog, embeddedModelCatalog} {
		var catalog modelCatalog
		if err := json.Unmarshal(data, &catalog); err != nil {
			return nil, fmt.Errorf("parse embedded Codex model catalog: %w", err)
		}
		for _, entry := range catalog.Models {
			slug, ok := entry["slug"].(string)
			if !ok || slug == "" {
				return nil, fmt.Errorf("embedded Codex model is missing a slug")
			}
			templates[slug] = entry
		}
	}
	return templates, nil
})

func buildModelCatalog(modelIDs []string) ([]byte, error) {
	templates, err := modelTemplates()
	if err != nil {
		return nil, err
	}
	selected := make([]map[string]any, 0, len(modelIDs))
	seen := make(map[string]bool, len(modelIDs))

	for _, id := range modelIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		template, ok := templates[model.CanonicalID(id)]
		if !ok {
			// Do not give an unrecognized model another model's instructions
			// or tool configuration.
			continue
		}

		entry := maps.Clone(template)
		entry["slug"] = id
		entry["display_name"] = model.Name(id)
		entry["visibility"] = "list"
		entry["priority"] = len(selected) + 1
		entry["availability_nux"] = nil
		entry["upgrade"] = nil
		entry["multi_agent_version"] = "v1"

		selected = append(selected, entry)
	}

	if len(selected) == 0 {
		return nil, fmt.Errorf("no available OpenAI models match the embedded Codex catalog; update Wingman or refresh the catalog with task generate:codex-models and rebuild")
	}

	return json.MarshalIndent(modelCatalog{Models: selected}, "", "  ")
}
