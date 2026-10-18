package harness

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/internal/provider"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

const (
	DefaultMaxTurns         = 400
	DefaultMaxParallelTools = 8
	DefaultToolTimeout      = 10 * time.Minute

	DefaultContextWindow = 400_000

	DefaultReserveTokens     = 32_000
	DefaultOutputTokenBudget = 64_000

	// Leave room for reasoning as well as extracted lists and code. This is
	// a ceiling; utility helpers can finish well below it.
	DefaultUtilityOutputTokenBudget = 64_000
)

func ContextWindowFor(id string) int {
	if value := strings.TrimSpace(os.Getenv("WINGMAN_CONTEXT_WINDOW")); value != "" {
		if window, err := strconv.Atoi(value); err == nil && window > 0 {
			return window
		}
	}

	if candidate, ok := model.Find(id); ok {
		window := candidate.ContextTokens()
		if window > 0 {
			return window
		}
	}

	return DefaultContextWindow
}

// outputTokenBudgetFor clamps a per-response allowance, including reasoning,
// to the model's advertised output limit. Unknown limits leave the provider
// default in effect unless an explicit budget is supplied.
func (c *Config) outputTokenBudgetFor(id string) int {
	budget := c.OutputTokenBudget
	m, ok := model.Find(id)
	if !ok || m.OutputTokens() <= 0 {
		return max(0, budget)
	}
	if budget <= 0 {
		budget = DefaultOutputTokenBudget
	}
	return min(budget, m.OutputTokens())
}

// ModelOption is a resolved model role. Efforts lists the supported reasoning
// efforts in ascending order; empty means unrestricted.
type ModelOption struct {
	ID      string
	Efforts []string
}

type Config struct {
	client *openai.Client

	// Telemetry instruments agent, model, and tool operations. ConfigFromEnvironment
	// initializes it when standard OTEL exporter variables are present. Library
	// callers may inject a separately configured pipeline instead. The host that
	// creates the pipeline must shut it down after all agents and workspaces close.
	Telemetry *telemetry.Telemetry

	Model        func() string
	Effort       func() string
	Tools        func() []tool.Tool
	Instructions func() string

	// PrepareTurn runs after reserving the session, before hooks and catalog
	// capture. It may prepare host resources or append context to the input.
	PrepareTurn func(context.Context, []Content) ([]Content, error)
	// CloseSession releases host-owned resources before SessionEnd hooks.
	// Neither callback is inherited by delegated agents.
	CloseSession func() error

	// PinTools snapshots Tools after PrepareTurn for the duration of a turn.
	// FilterTools applies current host policy to an owned copy of that catalog
	// at each request boundary. Leave PinTools false for a fully live catalog.
	PinTools    bool
	FilterTools func([]tool.Tool) []tool.Tool

	// PermissionMode reports the active mode for hooks at each request boundary.
	PermissionMode func() string

	// RequireFinish enables the explicit finish_turn protocol for a model.
	// Text-only replies cannot end Send when enabled. Structured output and
	// native refusals use their own completion semantics.
	RequireFinish func(model string) bool

	// ContextInstructions supplies mutable session guidance (environment,
	// project instructions, memory, skills). Changes append a replacement
	// snapshot to history instead of rewriting the cached Instructions prefix.
	ContextInstructions func() string

	// RoleModel resolves "default", "complex", and "utility" role models. An empty
	// role names the currently inherited model and is used for effort clamping.
	// ok=false or an empty ID keeps the inherited model. Nil disables role
	// overrides.
	RoleModel func(role string) (ModelOption, bool)

	// CacheKey routes provider-side prompt caching; keep it stable per
	// conversation (e.g. the session ID) to maximize prefix-cache hits.
	CacheKey string

	Hooks hook.Hooks

	// MaxTurns caps successful model invocations in one Send run. Stream
	// retries and tool calls do not consume turns. Zero uses the default;
	// negative disables the safety bound.
	MaxTurns int

	// OutputTokenBudget limits tokens generated per main-agent response,
	// including reasoning, and is clamped to a known model output limit.
	// Zero uses DefaultOutputTokenBudget for known limits and the provider
	// default otherwise. Utility helpers retain their own output budgets.
	OutputTokenBudget int

	// MaxTaskTokens limits cumulative reported input + output tokens per task,
	// including inline agents, retries and utility calls. Zero is unlimited.
	MaxTaskTokens int64
	// MaxTaskDuration includes inference, tools, retries and approval waits.
	// Zero is unlimited. ToolTimeout remains an independent per-tool limit.
	MaxTaskDuration time.Duration

	// MaxParallelTools bounds concurrently executing read-only tool calls.
	// Zero uses the default; negative allows the whole emitted batch.
	MaxParallelTools int

	// ToolTimeout is a hard ceiling on every tool call. When zero, tools may
	// extend the default via tool.Tool.Timeout; negative disables deadlines.
	ToolTimeout time.Duration

	ContextWindow int

	ReserveTokens int
}

func (c *Config) hookRuntime(ctx context.Context) hook.Runtime {
	runtime := hook.RuntimeFromContext(ctx)
	runtime.Model, runtime.ReasoningEffort = "", ""
	if c.Model != nil {
		runtime.Model = c.Model()
	}
	if c.Effort != nil {
		runtime.ReasoningEffort = c.Effort()
	}
	if c.PermissionMode != nil {
		runtime.PermissionMode = c.PermissionMode()
	}
	return runtime
}

// Clone copies the config with its own hook slices; client, telemetry and callbacks stay shared.
func (c *Config) Clone() *Config {
	clone := *c
	clone.Hooks = hook.Hooks{}
	clone.Hooks.Append(c.Hooks)
	return &clone
}

// Derive clones the config for a delegated agent, dropping hooks that belong to the top-level session.
func (c *Config) Derive() *Config {
	child := c.Clone()
	child.Hooks.UserPromptSubmit = nil
	child.Hooks.SessionStart = nil
	child.Hooks.SessionEnd = nil
	child.Hooks.Interrupt = nil
	child.PrepareTurn = nil
	child.CloseSession = nil
	return child
}

func (c *Config) utilityModelName() string {
	if c.RoleModel != nil {
		if option, ok := c.RoleModel("utility"); ok && option.ID != "" {
			return option.ID
		}
	}
	if c.Model != nil {
		return c.Model()
	}
	return ""
}

// Utility runs a one-shot completion on the utility model (falling back to the
// main model) and credits its token usage to the session through the context's
// usage sink.
func (c *Config) Utility(ctx context.Context, instructions, input string) (string, error) {
	modelID := c.utilityModelName()
	outputTokens := DefaultUtilityOutputTokenBudget
	if m, ok := model.Find(modelID); ok && m.OutputTokens() > 0 {
		outputTokens = min(outputTokens, m.OutputTokens())
	}
	result, err := c.Generate(ctx, GenerateOptions{
		Model:           modelID,
		Effort:          utilityEffort(modelID),
		Instructions:    instructions,
		Input:           input,
		MaxOutputTokens: int64(outputTokens),
	})
	reportUsage(ctx, result.Usage)
	return result.Text, err
}

func utilityEffort(modelID string) string {
	if m, ok := model.Find(modelID); ok && slices.Contains(m.Efforts, "low") {
		return "low"
	}
	return ""
}

func reportUsage(ctx context.Context, usage Usage) {
	tool.ReportUsage(ctx, tool.UsageDelta{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		ReasoningTokens:          usage.ReasoningTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
	})
}

func (c *Config) Models(ctx context.Context) ([]ModelInfo, error) {
	ids, err := provider.ModelIDs(ctx, *c.client)
	if err != nil {
		return nil, err
	}

	var models []ModelInfo

	for _, id := range ids {
		models = append(models, ModelInfo{ID: id})
	}

	return models, nil
}

// ConfigFromEnvironment builds a config from the environment; the caller owns its Telemetry.
func ConfigFromEnvironment() (*Config, error) {
	client := createClient()
	cfg := &Config{client: &client}
	if value := strings.TrimSpace(os.Getenv("WINGMAN_OUTPUT_TOKEN_BUDGET")); value != "" {
		budget, err := strconv.Atoi(value)
		if err != nil || budget < 0 {
			return nil, fmt.Errorf("WINGMAN_OUTPUT_TOKEN_BUDGET must be a nonnegative integer")
		}
		cfg.OutputTokenBudget = budget
	}
	if err := cfg.loadTaskLimits(); err != nil {
		return nil, err
	}
	tel, err := telemetry.NewFromEnvironment(context.Background(), defaultTelemetryOptions())
	if err != nil {
		return nil, fmt.Errorf("configure OpenTelemetry: %w", err)
	}

	cfg.Telemetry = tel

	if model := DefaultModel(); model != "" {
		cfg.Model = func() string { return model }
	}
	if effort := DefaultEffort(); effort != "" {
		cfg.Effort = func() string { return effort }
	}

	return cfg, nil
}

func defaultTelemetryOptions() telemetry.Options {
	baseURL, _ := clientConfig()
	provider := "wingman"
	switch {
	case envPresent("WINGMAN_URL"):
		provider = "wingman"
	case envPresent("OPENAI_API_KEY"):
		provider = "openai"
	case envPresent("OPENROUTER_API_KEY"):
		provider = "openrouter"
	case envPresent("OLLAMA_HOST") || envPresent("OLLAMA_API_KEY"):
		provider = "ollama"
	}

	opts := telemetry.Options{
		AgentName:    "wingman",
		ProviderName: provider,
	}
	if parsed, err := url.Parse(baseURL); err == nil {
		opts.ServerAddress = parsed.Hostname()
		if port := parsed.Port(); port != "" {
			opts.ServerPort, _ = strconv.Atoi(port)
		} else {
			switch strings.ToLower(parsed.Scheme) {
			case "http":
				opts.ServerPort = 80
			case "https":
				opts.ServerPort = 443
			}
		}
	}
	return opts
}

func envPresent(name string) bool {
	return strings.TrimSpace(os.Getenv(name)) != ""
}

// DefaultModel returns the model requested via environment; WINGMAN_MODEL
// takes priority over the OpenAI-standard OPENAI_DEFAULT_MODEL.
func DefaultModel() string {
	if v := os.Getenv("WINGMAN_MODEL"); v != "" {
		return v
	}

	return os.Getenv("OPENAI_DEFAULT_MODEL")
}

// DefaultComplexModel selects the model for explicit complex subagent tasks.
// WINGMAN_MODEL_PLAN remains a legacy alias; neither setting changes Plan mode.
func DefaultComplexModel() string {
	if v := os.Getenv("WINGMAN_MODEL_COMPLEX"); v != "" {
		return v
	}
	return os.Getenv("WINGMAN_MODEL_PLAN")
}

// DefaultUtilityModel returns the model for internal utility calls (recaps,
// compaction summaries); empty selects the smallest available automatically.
func DefaultUtilityModel() string {
	return os.Getenv("WINGMAN_MODEL_UTILITY")
}

// DefaultEffort returns the reasoning effort requested via WINGMAN_EFFORT.
// Empty (or "auto") leaves the model's default in place. Unrecognized
// values are ignored so a typo cannot silently pin an unexpected effort.
func DefaultEffort() string {
	return effortFromEnv("WINGMAN_EFFORT")
}

func effortFromEnv(name string) string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(name))); v {
	case "none", "low", "medium", "high", "xhigh", "max":
		return v
	default:
		return ""
	}
}

func createClient() openai.Client {
	baseURL, token := clientConfig()
	return openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(token),
	)
}

func clientConfig() (baseURL, token string) {
	providers := []func() (string, string, bool){
		wingmanConfig,
		openAIConfig,
		openRouterConfig,
		ollamaConfig,
	}

	for _, config := range providers {
		if baseURL, token, ok := config(); ok {
			return baseURL, token
		}
	}

	baseURL, token = provider.Wingman("", "")
	return baseURL + "/v1", token
}

func wingmanConfig() (baseURL, token string, ok bool) {
	if !envPresent("WINGMAN_URL") {
		return "", "", false
	}
	baseURL, token = provider.Wingman("", "")
	return baseURL + "/v1", token, true
}

func openAIConfig() (baseURL, token string, ok bool) {
	token = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if token == "" {
		return "", "", false
	}

	baseURL = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	return baseURL, token, true
}

func openRouterConfig() (baseURL, token string, ok bool) {
	token = strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if token == "" {
		return "", "", false
	}

	return "https://openrouter.ai/api/v1", token, true
}

func ollamaConfig() (baseURL, token string, ok bool) {
	host := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	token = strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	if host == "" && token == "" {
		return "", "", false
	}
	if host == "" {
		host = "https://ollama.com"
	} else if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	if token == "" {
		token = "-"
	}

	return strings.TrimRight(host, "/") + "/v1", token, true
}
