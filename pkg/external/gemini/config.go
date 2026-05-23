package gemini

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

type Options = external.Options

type GeminiConfig struct {
	BaseURL   string
	AuthToken string

	Model string
}

func NewConfig(ctx context.Context, options *Options) (*GeminiConfig, error) {
	options = external.WithDefaults(options)

	models, err := external.Models(ctx, options, &external.ModelOptions{
		Kind:   external.ModelDefault,
		Filter: external.IsGoogle,
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
