package claude

import (
	"context"
	"strings"

	"github.com/adrianliechti/wingman-agent/tui/run"
)

type Options = run.Options

type ClaudeConfig struct {
	BaseURL   string
	AuthToken string

	OpusModel   string
	HaikuModel  string
	SonnetModel string

	ContextWindow int
}

func NewConfig(ctx context.Context, options *Options) (*ClaudeConfig, error) {
	options = run.WithDefaults(options)

	available, err := run.AvailableModels(ctx, options)

	if err != nil {
		return nil, err
	}

	pick := func(name string) string {
		return run.Pick(available, func(id string) bool {
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
