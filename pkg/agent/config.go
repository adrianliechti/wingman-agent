package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const (
	DefaultMaxTurns    = 200
	DefaultToolTimeout = 10 * time.Minute

	DefaultContextWindow = 400_000

	DefaultReserveTokens = 32_000
)

type Config struct {
	client *openai.Client

	Model        func() string
	Effort       func() string
	Tools        func() []tool.Tool
	Instructions func() string

	Hooks hook.Hooks

	MaxTurns int

	// ToolTimeout is a hard ceiling on every tool call. When zero, tools may
	// extend the default via tool.Tool.Timeout; negative disables deadlines.
	ToolTimeout time.Duration

	ContextWindow int

	ReserveTokens int
}

func (c *Config) Derive() *Config {
	return &Config{
		client: c.client,
		Model:  c.Model,
		Effort: c.Effort,

		Hooks: hook.Hooks{
			PreToolUse:  slices.Clone(c.Hooks.PreToolUse),
			PostToolUse: slices.Clone(c.Hooks.PostToolUse),
		},

		MaxTurns:    c.MaxTurns,
		ToolTimeout: c.ToolTimeout,

		ContextWindow: c.ContextWindow,
		ReserveTokens: c.ReserveTokens,
	}
}

func (c *Config) Models(ctx context.Context) ([]ModelInfo, error) {
	resp, err := c.client.Models.List(ctx)
	if err != nil {
		return nil, err
	}

	var models []ModelInfo

	for _, m := range resp.Data {
		models = append(models, ModelInfo{ID: m.ID})
	}

	return models, nil
}

func DefaultConfig() (*Config, error) {
	client := createClient()

	return &Config{
		client: &client,
	}, nil
}

func createClient() openai.Client {
	if url, ok := os.LookupEnv("WINGMAN_URL"); ok {
		baseURL := strings.TrimRight(url, "/") + "/v1"

		token, _ := os.LookupEnv("WINGMAN_TOKEN")

		if token == "" {
			token = "-"
		}

		return openai.NewClient(
			option.WithBaseURL(baseURL),
			option.WithAPIKey(token),
		)
	}

	if token, ok := os.LookupEnv("OPENAI_API_KEY"); ok {
		baseURL := "https://api.openai.com/v1"

		if url, ok := os.LookupEnv("OPENAI_BASE_URL"); ok {
			baseURL = url
		}

		return openai.NewClient(
			option.WithBaseURL(baseURL),
			option.WithAPIKey(token),
		)
	}

	return openai.NewClient(
		option.WithBaseURL("http://localhost:8080/v1"),
		option.WithAPIKey("-"),
	)
}
