package copilot

import (
	"context"

	"github.com/adrianliechti/wingman-agent/tui/run"
)

type Options = run.Options

type CopilotConfig struct {
	BaseURL   string
	AuthToken string

	Model string

	MaxPromptTokens int
	MaxOutputTokens int
}

func NewConfig(ctx context.Context, options *Options) (*CopilotConfig, error) {
	options = run.WithDefaults(options)

	models, err := run.Models(ctx, options, &run.ModelOptions{
		Kind:   run.ModelDefault,
		Filter: run.IsOpenAI,
	})

	if err != nil {
		return nil, err
	}

	cfg := &CopilotConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,

		MaxPromptTokens: 400000,
		MaxOutputTokens: 128000,
	}

	if len(models) > 0 {
		cfg.Model = models[0]
	}

	return cfg, nil
}
