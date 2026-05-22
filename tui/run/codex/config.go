package codex

import (
	"context"

	"github.com/adrianliechti/wingman-agent/tui/run"
)

type Options = run.Options

type CodexConfig struct {
	BaseURL   string
	AuthToken string

	Model string
}

func NewConfig(ctx context.Context, options *Options) (*CodexConfig, error) {
	options = run.WithDefaults(options)

	models, err := run.Models(ctx, options, &run.ModelOptions{
		Kind:   run.ModelDefault,
		Filter: run.IsOpenAI,
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
