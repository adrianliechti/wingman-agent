package claude

import (
	"context"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

type Options = external.Options

type ClaudeConfig struct {
	BaseURL   string
	AuthToken string

	OpusModel   string
	HaikuModel  string
	SonnetModel string

	ContextWindow int
}

func NewConfig(ctx context.Context, options *Options) (*ClaudeConfig, error) {
	options = external.WithDefaults(options)

	available, err := external.AvailableModels(ctx, options)

	if err != nil {
		return nil, err
	}

	pick := func(name string) string {
		return external.Pick(available, func(id string) bool {
			return strings.Contains(id, name)
		})
	}

	return &ClaudeConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,

		HaikuModel:  pick("haiku"),
		SonnetModel: pick("sonnet"),
		OpusModel:   pick("opus"),
	}, nil
}
