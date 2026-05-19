package agent

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const (
	// DefaultMaxTurns bounds the internal model→tools→model loop within a
	// single Send(). Each model invocation counts as one — a response that
	// fires 10 parallel tool calls is still one tick. Big audits and refactors
	// routinely take 30-80 rounds; 200 leaves comfortable headroom while
	// still catching runaway tool-call cycles before they burn many tokens.
	DefaultMaxTurns    = 200
	DefaultToolTimeout = 10 * time.Minute

	// DefaultContextWindow is the input-token capacity assumed when the
	// Config.ContextWindow override is zero. 400K is a deliberate middle
	// ground: it covers GPT-5 at face value, and on 1M-default models
	// (modern Claude family, GPT-5.5) it caps usable context at 350K — a
	// trade against the "lost in the middle" / long-context retrieval
	// degradation that shows up well before 1M is reached. Users who want
	// the full 1M should set Config.ContextWindow = 1_000_000 explicitly.
	// Smaller-window models (200K Claude, 128K GPT-4o) need a smaller
	// override so proactive compaction fires before the API rejects.
	DefaultContextWindow = 400_000

	// DefaultReserveTokens is the headroom kept free for the next request's
	// new content (model response + appended tool results). At the default
	// 400K window this puts the proactive trigger at 368K (~8% headroom) —
	// enough to absorb a typical turn (a handful of tool calls with the
	// truncation hook's soft caps in play, plus the model response). A
	// pathologically heavy turn can still overflow; the reactive recovery
	// path in recovery.go catches that at the cost of one wasted round-trip.
	DefaultReserveTokens = 32_000
)

type Config struct {
	client *openai.Client

	Model        func() string
	Effort       func() string
	Tools        func() []tool.Tool
	Instructions func() string

	Hooks hook.Hooks

	// MaxTurns is a safety bound on the internal model→tool→model loop
	// within a single Send(), to catch runaway tool-call cycles. It is NOT
	// a user-visible conversational turn count — a single user message can
	// legitimately drive dozens of tool calls. Zero applies the default;
	// negative disables.
	MaxTurns    int
	ToolTimeout time.Duration

	// ContextWindow is the model's input-token capacity. The loop summarizes
	// older messages proactively when the last turn's input would otherwise
	// approach this limit. Zero applies DefaultContextWindow; negative
	// disables proactive compaction (the reactive on-error path still runs).
	ContextWindow int

	// ReserveTokens is the headroom kept free for the upcoming model
	// response and appended tool results. Compaction fires when the prior
	// turn's input exceeds (ContextWindow - ReserveTokens). Zero applies
	// DefaultReserveTokens.
	ReserveTokens int
}

func (c *Config) Derive() *Config {
	return &Config{
		client: c.client,
		Model:  c.Model,
		Effort: c.Effort,
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
