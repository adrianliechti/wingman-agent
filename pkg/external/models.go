package external

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/internal/provider"
	"github.com/adrianliechti/wingman-agent/pkg/model"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type ModelKind int

const (
	ModelDefault ModelKind = iota
	ModelFast
)

type ModelFilter func(id string) bool

func IsAnthropic(id string) bool {
	return strings.HasPrefix(strings.ToLower(model.CanonicalID(id)), "claude-")
}

func IsOpenAI(id string) bool {
	return strings.HasPrefix(strings.ToLower(model.CanonicalID(id)), "gpt-")
}

func IsGoogle(id string) bool {
	return strings.HasPrefix(strings.ToLower(model.CanonicalID(id)), "gemini-")
}

type ModelOptions struct {
	Kind   ModelKind
	Filter ModelFilter
}

func WithDefaults(options *Options) *Options {
	if options == nil {
		options = new(Options)
	}

	options.WingmanURL, options.WingmanToken = provider.Wingman(options.WingmanURL, options.WingmanToken)

	return options
}

func AvailableModels(ctx context.Context, options *Options) (map[string]bool, error) {
	options = WithDefaults(options)

	client := openai.NewClient(
		option.WithBaseURL(strings.TrimRight(options.WingmanURL, "/")+"/v1"),
		option.WithAPIKey(options.WingmanToken),
	)

	ids, err := provider.ModelIDs(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("Wingman model discovery at %s failed; check that the gateway is running and WINGMAN_URL/WINGMAN_TOKEN are correct: %w", options.WingmanURL, err)
	}
	available := make(map[string]bool, len(ids))
	for _, id := range ids {
		available[id] = true
	}
	return available, nil
}

func Models(ctx context.Context, options *Options, modelOpts *ModelOptions) ([]string, error) {
	available, err := AvailableModels(ctx, options)

	if err != nil {
		return nil, err
	}

	if modelOpts == nil {
		modelOpts = new(ModelOptions)
	}

	var out []string

	for _, m := range model.Available(available) {
		switch modelOpts.Kind {
		case ModelFast:
			if m.Class != model.ClassSmall {
				continue
			}

		default:
			if m.Class == model.ClassSmall {
				continue
			}
		}

		if modelOpts.Filter != nil && !modelOpts.Filter(m.ID) {
			continue
		}

		out = append(out, m.ID)
	}

	return out, nil
}
