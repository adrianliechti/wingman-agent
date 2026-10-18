package antigravity

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/external"
	"github.com/adrianliechti/wingman-agent/pkg/model"
)

type Options = external.Options

type AntigravityConfig struct {
	BaseURL   string
	AuthToken string

	Model  string
	Models []model.Model
}

func NewConfig(ctx context.Context, options *Options) (*AntigravityConfig, error) {
	options = external.WithDefaults(options)

	available, err := external.AvailableModels(ctx, options)

	if err != nil {
		return nil, err
	}

	models := model.Available(available)

	return &AntigravityConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,

		Model:  defaultModel(models),
		Models: models,
	}, nil
}

// Antigravity's prompts are written for Gemini, so a Gemini model is preferred.
func defaultModel(models []model.Model) string {
	var fallback string

	for _, m := range models {
		if m.Class == model.ClassSmall {
			continue
		}

		if external.IsGoogle(m.ID) {
			return m.ID
		}

		if fallback == "" {
			fallback = m.ID
		}
	}

	if fallback == "" && len(models) > 0 {
		return models[0].ID
	}

	return fallback
}
