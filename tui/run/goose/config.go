package goose

import (
	"context"
	"os"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/tui/run"
)

type Options = run.Options

type GooseConfig struct {
	BaseURL   string
	AuthToken string

	Model     string
	FastModel string

	ContextLimit int
}

func NewConfig(ctx context.Context, options *Options) (*GooseConfig, error) {
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

	cfg := &GooseConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,

		ContextLimit: 200000,
	}

	pick := func(candidates ...string) string {
		for _, id := range candidates {
			if available[id] {
				return id
			}
		}
		return ""
	}

	cfg.Model = pick(
		// Claude models
		"claude-sonnet-4-6",
		"claude-sonnet-4-5",
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-opus-4-5",

		// ChatGPT models
		"gpt-5.5",
		"gpt-5.4",

		// Codex models
		"gpt-5.3-codex",
		"gpt-5.2-codex",
		"gpt-5.1-codex-max",
		"gpt-5.1-codex",
		"gpt-5-codex",

		// Legacy ChatGPT models
		"gpt-5.2",
		"gpt-5.1",
		"gpt-5",

		// Gemini Pro models
		"gemini-3.1-pro-preview",
		"gemini-3-pro-preview",
		"gemini-2.5-pro",
	)

	cfg.FastModel = pick(
		// Haiku models
		"claude-haiku-4-6",
		"claude-haiku-4-5",

		// Mini / Spark models
		"gpt-5.3-codex-spark",
		"gpt-5.1-codex-mini",
		"gpt-5-mini",

		// Flash models
		"gemini-3-flash-preview",
		"gemini-2.5-flash",
	)

	return cfg, nil
}
