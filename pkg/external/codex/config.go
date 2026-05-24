package codex

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

type Options = external.Options

type CodexConfig struct {
	BaseURL   string
	AuthToken string

	Model string
}

func NewConfig(ctx context.Context, options *Options) (*CodexConfig, error) {
	options = external.WithDefaults(options)

	models, err := external.Models(ctx, options, &external.ModelOptions{
		Kind:   external.ModelDefault,
		Filter: external.IsOpenAI,
	})

	if err != nil {
		return nil, err
	}

	cfg := &CodexConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,
	}

	if len(models) > 0 {
		cfg.Model = models[0]
	}

	return cfg, nil
}
