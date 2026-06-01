package external

import (
	"context"
	"os"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type ModelKind int

const (
	ModelDefault ModelKind = iota
	ModelFast
)

// ModelFilter narrows a candidate model list by ID. A nil filter matches
// every model.
type ModelFilter func(id string) bool

// IsAnthropic matches Anthropic Claude model IDs.
func IsAnthropic(id string) bool { return strings.HasPrefix(id, "claude-") }

// IsOpenAI matches OpenAI GPT model IDs.
func IsOpenAI(id string) bool { return strings.HasPrefix(id, "gpt-") }

// IsGoogle matches Google Gemini model IDs.
func IsGoogle(id string) bool { return strings.HasPrefix(id, "gemini-") }

type ModelOptions struct {
	Kind   ModelKind
	Filter ModelFilter
}

// WithDefaults fills in any missing Options fields from environment variables.
// The same pointer is returned for chaining; a new zero-value Options is
// allocated if nil is passed.
func WithDefaults(options *Options) *Options {
	if options == nil {
		options = new(Options)
	}

	if options.WingmanURL == "" {
		val := os.Getenv("WINGMAN_URL")

		if val == "" {
			val = "http://localhost:4242"
		}

		options.WingmanURL = val
	}

	if options.WingmanToken == "" {
		val := os.Getenv("WINGMAN_TOKEN")

		if val == "" {
			val = "-"
		}

		options.WingmanToken = val
	}

	return options
}

// AvailableModels returns the set of model IDs advertised by the Wingman
// server addressed by options.
func AvailableModels(ctx context.Context, options *Options) (map[string]bool, error) {
	options = WithDefaults(options)

	client := openai.NewClient(
		option.WithBaseURL(strings.TrimRight(options.WingmanURL, "/")+"/v1"),
		option.WithAPIKey(options.WingmanToken),
	)

	iter := client.Models.ListAutoPaging(ctx)

	available := make(map[string]bool)

	for iter.Next() {
		available[iter.Current().ID] = true
	}

	if err := iter.Err(); err != nil {
		return nil, err
	}

	return available, nil
}

// Models returns the IDs of available models matching modelOpts, in
// preference order. Returns nil if nothing matches.
func Models(ctx context.Context, options *Options, modelOpts *ModelOptions) ([]string, error) {
	available, err := AvailableModels(ctx, options)

	if err != nil {
		return nil, err
	}

	if modelOpts == nil {
		modelOpts = new(ModelOptions)
	}

	candidates := defaultModels

	if modelOpts.Kind == ModelFast {
		candidates = fastModels
	}

	var out []string

	for _, id := range candidates {
		if !available[id] {
			continue
		}

		if modelOpts.Filter != nil && !modelOpts.Filter(id) {
			continue
		}

		out = append(out, id)
	}

	return out, nil
}

var (
	defaultModels = []string{
		// Anthropic
		"claude-sonnet-4-6",
		"claude-sonnet-4-5",
		"claude-opus-4-8",
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-opus-4-5",

		// OpenAI
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.3-codex",
		"gpt-5.2-codex",
		"gpt-5.1-codex-max",
		"gpt-5.1-codex",
		"gpt-5-codex",
		"gpt-5.2",
		"gpt-5.1",
		"gpt-5",

		// Google
		"gemini-3.1-pro-preview",
		"gemini-3-pro-preview",
		"gemini-2.5-pro",
	}

	fastModels = []string{
		// Anthropic
		"claude-haiku-4-6",
		"claude-haiku-4-5",

		// OpenAI
		"gpt-5.3-codex-spark",
		"gpt-5.1-codex-mini",
		"gpt-5-mini",

		// Google
		"gemini-3-flash-preview",
		"gemini-2.5-flash",
	}
)
