package junie

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

type Options = external.Options

type JunieConfig struct {
	BaseURL   string
	AuthToken string

	Model     string
	FastModel string
}

func NewConfig(ctx context.Context, options *Options) (*JunieConfig, error) {
	options = external.WithDefaults(options)

	defaultModels, err := external.Models(ctx, options, &external.ModelOptions{
		Kind:   external.ModelDefault,
		Filter: external.IsOpenAI,
	})

	if err != nil {
		return nil, err
	}

	fastModels, err := external.Models(ctx, options, &external.ModelOptions{
		Kind:   external.ModelFast,
		Filter: external.IsOpenAI,
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
