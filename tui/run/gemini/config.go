package gemini

import (
	"context"

	"github.com/adrianliechti/wingman-agent/tui/run"
)

type Options = run.Options

type GeminiConfig struct {
	BaseURL   string
	AuthToken string

	Model string
}

func NewConfig(ctx context.Context, options *Options) (*GeminiConfig, error) {
	options = run.WithDefaults(options)

	models, err := run.Models(ctx, options, &run.ModelOptions{
		Kind:   run.ModelDefault,
		Filter: run.IsGoogle,
	})

	if err != nil {
		return nil, err
	}

	cfg := &GeminiConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,
	}

	if len(models) > 0 {
		cfg.Model = models[0]
	}

	return cfg, nil
}
