package claude

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/external"
	"github.com/adrianliechti/wingman-agent/pkg/model"
)

type Options = external.Options

type ClaudeConfig struct {
	BaseURL   string
	AuthToken string

	Models       []string
	DefaultModel string

	OpusModel   string
	HaikuModel  string
	SonnetModel string
	FableModel  string
}

func NewConfig(ctx context.Context, options *Options) (*ClaudeConfig, error) {
	options = external.WithDefaults(options)

	available, err := external.AvailableModels(ctx, options)
	if err != nil {
		return nil, err
	}

	cfg := &ClaudeConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,
	}
	// Allow every ID native gateway discovery offers, including models newer
	// than Wingman's catalog.
	for id, enabled := range available {
		lower := strings.ToLower(id)
		if enabled && (strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic")) {
			cfg.Models = append(cfg.Models, id)
		}
	}
	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("gateway has no available Claude models")
	}
	slices.Sort(cfg.Models)

	// Use the catalog for preferred defaults.
	for _, m := range model.Available(available) {
		if !external.IsAnthropic(m.ID) {
			continue
		}
		if cfg.DefaultModel == "" && m.Class != model.ClassSmall {
			cfg.DefaultModel = m.ID
		}
		id := model.CanonicalID(m.ID)
		switch {
		case strings.HasPrefix(id, "claude-haiku-") && cfg.HaikuModel == "":
			cfg.HaikuModel = m.ID
		case strings.HasPrefix(id, "claude-sonnet-") && cfg.SonnetModel == "":
			cfg.SonnetModel = m.ID
		case strings.HasPrefix(id, "claude-opus-") && cfg.OpusModel == "":
			cfg.OpusModel = m.ID
		case strings.HasPrefix(id, "claude-fable-") && cfg.FableModel == "":
			cfg.FableModel = m.ID
		}
	}

	cfg.DefaultModel = cmp.Or(cfg.DefaultModel, cfg.HaikuModel, cfg.Models[0])

	return cfg, nil
}
