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

// modelContextWindows maps model-ID prefixes to two compaction budgets:
// window stays under the provider's long-context price threshold; large is
// the full hardware window for models where it exceeds that threshold.
// Verified 2026-07: current Claude models (Opus 4.6+, Sonnet 4.6+, Fable 5)
// take 1M input tokens at flat per-token rates — no long-context premium;
// Haiku and pre-4.6 models are 200k hardware (Sonnet 4.5's 1M beta bills 2x
// above 200k and needs a beta header, so it stays capped). GPT-5.4/5.5 have
// 1M-class windows but bill 2x input / 1.5x output for the whole session
// once input exceeds 272k; GPT-5.6 (sol/terra/luna) keeps the short/long
// pricing split with an unpublished threshold, so it inherits the 272k
// budget. Codex and earlier GPT-5.x are 400k total, flat.
// Gemini bills ~2x above 200k prompts.
var modelContextWindows = []struct {
	prefix string
	window int // budget under the long-context price threshold
	large  int // full hardware window when it exceeds the budget (0 = same)
}{
	{"claude-haiku", 200_000, 0},
	{"claude-opus-4-5", 200_000, 0},
	{"claude-opus-4-1", 200_000, 0},
	{"claude-opus-4-0", 200_000, 0},
	{"claude-sonnet-4-5", 200_000, 0},
	{"claude-sonnet-4-0", 200_000, 0},
	{"claude-3", 200_000, 0},
	{"claude-", 1_000_000, 0},

	{"gpt-5.6", 272_000, 1_000_000},
	{"gpt-5.5", 272_000, 1_000_000},
	{"gpt-5.4", 272_000, 1_000_000},
	{"gpt-5", 400_000, 0},
	{"gpt-4.1", 1_000_000, 0},
	{"gpt-4o", 128_000, 0},
	{"o3", 200_000, 0},
	{"o4", 200_000, 0},

	{"gemini-", 200_000, 1_000_000},
}

func ContextWindowFor(model string, largeContext bool) int {
	model = strings.ToLower(model)

	for _, e := range modelContextWindows {
		if strings.HasPrefix(model, e.prefix) {
			if largeContext && e.large > e.window {
				return e.large
			}
			return e.window
		}
	}

	return DefaultContextWindow
}

type Config struct {
	client *openai.Client

	Model        func() string
	Effort       func() string
	Tools        func() []tool.Tool
	Instructions func() string

	// CacheKey routes provider-side prompt caching; keep it stable per
	// conversation (e.g. the session ID) to maximize prefix-cache hits.
	CacheKey string

	Hooks hook.Hooks

	MaxTurns int

	// ToolTimeout is a hard ceiling on every tool call. When zero, tools may
	// extend the default via tool.Tool.Timeout; negative disables deadlines.
	ToolTimeout time.Duration

	ContextWindow int

	// LargeContext compacts against the model's full hardware window instead
	// of stopping at the provider's long-context price threshold (e.g. 2x
	// input pricing on GPT-5.4/5.5 beyond 272k input tokens).
	LargeContext bool

	ReserveTokens int
}

func (c *Config) Derive() *Config {
	return &Config{
		client: c.client,
		Model:  c.Model,
		Effort: c.Effort,

		CacheKey: c.CacheKey,

		Hooks: hook.Hooks{
			PreToolUse:  slices.Clone(c.Hooks.PreToolUse),
			PostToolUse: slices.Clone(c.Hooks.PostToolUse),
		},

		MaxTurns:    c.MaxTurns,
		ToolTimeout: c.ToolTimeout,

		ContextWindow: c.ContextWindow,
		LargeContext:  c.LargeContext,
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

	cfg := &Config{
		client:       &client,
		LargeContext: envBool("WINGMAN_LARGE_CONTEXT"),
	}

	if model := DefaultModel(); model != "" {
		cfg.Model = func() string { return model }
	}

	return cfg, nil
}

// DefaultModel returns the model requested via environment; WINGMAN_MODEL
// takes priority over the OpenAI-standard OPENAI_DEFAULT_MODEL.
func DefaultModel() string {
	if v := os.Getenv("WINGMAN_MODEL"); v != "" {
		return v
	}

	return os.Getenv("OPENAI_DEFAULT_MODEL")
}

func envBool(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
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
