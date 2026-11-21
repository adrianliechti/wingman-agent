package goose

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/external"
	"github.com/adrianliechti/wingman-agent/pkg/model"
)

type Options = external.Options

type GooseConfig struct {
	BaseURL   string
	AuthToken string

	Model     string
	FastModel string

	ContextLimit int
}

func NewConfig(ctx context.Context, options *Options) (*GooseConfig, error) {
	options = external.WithDefaults(options)

	available, err := external.AvailableModels(ctx, options)
	if err != nil {
		return nil, err
	}

	cfg := &GooseConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,

		ContextLimit: 200000,
	}

	models := external.LauncherModels(available)
	if len(models) == 0 {
		return cfg, nil
	}
	main := models[0]
	for _, m := range models {
		if m.Class == model.ClassSmall {
			if cfg.FastModel == "" {
				cfg.FastModel = m.ID
			}
		} else if main.Class == model.ClassSmall {
			main = m
		}
	}
	cfg.Model = main.ID
	cfg.ContextLimit = main.ContextTokens()

	return cfg, nil
}
