package junie

import (
	"context"

	"github.com/adrianliechti/wingman-agent/tui/run"
)

type Options = run.Options

type JunieConfig struct {
	BaseURL   string
	AuthToken string

	Model     string
	FastModel string
}

func NewConfig(ctx context.Context, options *Options) (*JunieConfig, error) {
	options = run.WithDefaults(options)

	defaultModels, err := run.Models(ctx, options, &run.ModelOptions{
		Kind:   run.ModelDefault,
		Filter: run.IsOpenAI,
	})

	if err != nil {
		return nil, err
	}

	fastModels, err := run.Models(ctx, options, &run.ModelOptions{
		Kind:   run.ModelFast,
		Filter: run.IsOpenAI,
	})

	if err != nil {
		return nil, err
	}

	cfg := &JunieConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,
	}

	if len(defaultModels) > 0 {
		cfg.Model = defaultModels[0]
	}

	if len(fastModels) > 0 {
		cfg.FastModel = fastModels[0]
	}

	return cfg, nil
}
