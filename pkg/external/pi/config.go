package pi

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

type Options = external.Options

type PiConfig struct {
	BaseURL   string
	AuthToken string

	Model  string
	Models []string
}

func NewConfig(ctx context.Context, options *Options) (*PiConfig, error) {
	options = external.WithDefaults(options)

	defaults, err := external.Models(ctx, options, &external.ModelOptions{
		Kind: external.ModelDefault,
	})

	if err != nil {
		return nil, err
	}

	fast, err := external.Models(ctx, options, &external.ModelOptions{
		Kind: external.ModelFast,
	})

	if err != nil {
		return nil, err
	}

	cfg := &PiConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,

		Models: append(defaults, fast...),
	}

	if len(defaults) > 0 {
		cfg.Model = defaults[0]
	} else if len(cfg.Models) > 0 {
		cfg.Model = cfg.Models[0]
	}

	return cfg, nil
}
