package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
)

const (
	defaultTimeout = 30
	maxHookOutput  = 16 * 1024
)

// Gate defers a yes/no decision (e.g. "trust this workspace's hooks?") until
// the first hook actually fires, then remembers it for the session.
type Gate struct {
	Confirm func(ctx context.Context, message string) (bool, error)
	Message string

	mu      sync.Mutex
	decided bool
	allowed bool
}

func (g *Gate) Allowed(ctx context.Context) bool {
	if g == nil {
		return true
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.decided {
		return g.allowed
	}
	g.decided = true

	if g.Confirm == nil {
		g.allowed = true
		return true
	}

	ok, err := g.Confirm(ctx, g.Message)
	g.allowed = ok && err == nil
	return g.allowed
}

type Config struct {
	PreToolUse       []Rule `json:"preToolUse,omitempty"`
	PostToolUse      []Rule `json:"postToolUse,omitempty"`
	UserPromptSubmit []Rule `json:"userPromptSubmit,omitempty"`
	SessionStart     []Rule `json:"sessionStart,omitempty"`
	SessionEnd       []Rule `json:"sessionEnd,omitempty"`
	SubagentStop     []Rule `json:"subagentStop,omitempty"`
	PreCompact       []Rule `json:"preCompact,omitempty"`
}

type Rule struct {
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
	Once    bool   `json:"once,omitempty"`
}

func (r Rule) empty() bool {
	return r.Command == "" && r.URL == ""
}

// RuleCount reports how many hook rules the config carries, used for the
// workspace trust prompt.
func (c *Config) RuleCount() int {
	return len(c.PreToolUse) + len(c.PostToolUse) + len(c.UserPromptSubmit) +
		len(c.SessionStart) + len(c.SessionEnd) + len(c.SubagentStop) + len(c.PreCompact)
}

func Load(paths ...string) (*Config, error) {
	cfg := &Config{}
	var errs []error

	for _, path := range paths {
		if path == "" {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var c Config
		if err := json.Unmarshal(data, &c); err != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", path, err))
			continue
		}

		cfg.PreToolUse = append(cfg.PreToolUse, c.PreToolUse...)
		cfg.PostToolUse = append(cfg.PostToolUse, c.PostToolUse...)
		cfg.UserPromptSubmit = append(cfg.UserPromptSubmit, c.UserPromptSubmit...)
		cfg.SessionStart = append(cfg.SessionStart, c.SessionStart...)
		cfg.SessionEnd = append(cfg.SessionEnd, c.SessionEnd...)
		cfg.SubagentStop = append(cfg.SubagentStop, c.SubagentStop...)
		cfg.PreCompact = append(cfg.PreCompact, c.PreCompact...)
	}

	return cfg, errors.Join(errs...)
}

func (c *Config) PreHooks(workDir string, gate *Gate) []hook.PreToolUse {
	var hooks []hook.PreToolUse

	for _, rule := range c.PreToolUse {
		if rule.empty() {
			continue
		}
		fire := rule.limiter()

		hooks = append(hooks, func(ctx context.Context, call tool.ToolCall) (string, error) {
			if !rule.matches(call.Name) || !fire() || !gate.Allowed(ctx) {
				return "", nil
			}

			out, err := rule.run(ctx, workDir, payload{Event: "pre_tool_use", ToolName: call.Name, Args: rawArgs(call)})
			if err != nil {
				if out == "" {
					out = err.Error()
				}
				return fmt.Sprintf("blocked by pre-tool hook (%s): %s", rule.name(), out), nil
			}

			return "", nil
		})
	}

	return hooks
}

func (c *Config) PostHooks(workDir string, gate *Gate) []hook.PostToolUse {
	var hooks []hook.PostToolUse

	for _, rule := range c.PostToolUse {
		if rule.empty() {
			continue
		}
		fire := rule.limiter()

		hooks = append(hooks, func(ctx context.Context, call tool.ToolCall, result string) (string, error) {
			if !rule.matches(call.Name) || tool.IsImageResult(result) || !fire() || !gate.Allowed(ctx) {
				return result, nil
			}

			out, err := rule.run(ctx, workDir, payload{Event: "post_tool_use", ToolName: call.Name, Args: rawArgs(call), Result: result})
			if err != nil || out == "" {
				return result, nil
			}

			if len(out) > maxHookOutput {
				out = out[:maxHookOutput] + "\n[hook output truncated]"
			}
			return result + "\n\n<hook-output>\n" + out + "\n</hook-output>", nil
		})
	}

	return hooks
}

func (c *Config) PromptHooks(workDir string, gate *Gate) []hook.UserPromptSubmit {
	var hooks []hook.UserPromptSubmit

	for _, rule := range c.UserPromptSubmit {
		if rule.empty() {
			continue
		}
		fire := rule.limiter()

		hooks = append(hooks, func(ctx context.Context, prompt string) (string, error) {
			if !fire() || !gate.Allowed(ctx) {
				return "", nil
			}

			out, err := rule.run(ctx, workDir, payload{Event: "user_prompt_submit", Prompt: prompt})
			if err != nil {
				if out == "" {
					out = err.Error()
				}
				return "", fmt.Errorf("blocked by prompt hook (%s): %s", rule.name(), out)
			}

			return out, nil
		})
	}

	return hooks
}

func (c *Config) StartHooks(workDir string, gate *Gate) []hook.SessionStart {
	var hooks []hook.SessionStart

	for _, rule := range c.SessionStart {
		if rule.empty() {
			continue
		}
		fire := rule.limiter()

		hooks = append(hooks, func(ctx context.Context) (string, error) {
			if !fire() || !gate.Allowed(ctx) {
				return "", nil
			}

			out, err := rule.run(ctx, workDir, payload{Event: "session_start"})
			if err != nil {
				return "", nil
			}
			return out, nil
		})
	}

	return hooks
}

func (c *Config) EndHooks(workDir string, gate *Gate) []hook.SessionEnd {
	var hooks []hook.SessionEnd

	for _, rule := range c.SessionEnd {
		if rule.empty() {
			continue
		}
		fire := rule.limiter()

		hooks = append(hooks, func(ctx context.Context) {
			if !fire() || !gate.Allowed(ctx) {
				return
			}
			rule.run(ctx, workDir, payload{Event: "session_end"})
		})
	}

	return hooks
}

func (c *Config) SubagentHooks(workDir string, gate *Gate) []hook.SubagentStop {
	var hooks []hook.SubagentStop

	for _, rule := range c.SubagentStop {
		if rule.empty() {
			continue
		}
		fire := rule.limiter()

		hooks = append(hooks, func(ctx context.Context, agentType, result string) {
			if !rule.matches(agentType) || !fire() || !gate.Allowed(ctx) {
				return
			}
			rule.run(ctx, workDir, payload{Event: "subagent_stop", AgentType: agentType, Result: result})
		})
	}

	return hooks
}

func (c *Config) CompactHooks(workDir string, gate *Gate) []hook.PreCompact {
	var hooks []hook.PreCompact

	for _, rule := range c.PreCompact {
		if rule.empty() {
			continue
		}
		fire := rule.limiter()

		hooks = append(hooks, func(ctx context.Context) error {
			if !fire() || !gate.Allowed(ctx) {
				return nil
			}

			out, err := rule.run(ctx, workDir, payload{Event: "pre_compact"})
			if err != nil {
				if out == "" {
					out = err.Error()
				}
				return fmt.Errorf("compaction blocked by hook (%s): %s", rule.name(), out)
			}
			return nil
		})
	}

	return hooks
}

func (r Rule) matches(name string) bool {
	matcher := strings.TrimSpace(r.Matcher)
	if matcher == "" || matcher == "*" {
		return true
	}
	for part := range strings.SplitSeq(matcher, ",") {
		if strings.TrimSpace(part) == name {
			return true
		}
	}
	return false
}

// name identifies the rule in messages shown to the user or model.
func (r Rule) name() string {
	if r.Command != "" {
		return r.Command
	}
	return r.URL
}

// limiter returns a firing guard honoring Once for this built hook instance.
func (r Rule) limiter() func() bool {
	if !r.Once {
		return func() bool { return true }
	}
	var fired atomic.Bool
	return func() bool { return fired.CompareAndSwap(false, true) }
}

func (r Rule) run(ctx context.Context, workDir string, p payload) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	input, _ := json.Marshal(p)

	if r.URL != "" {
		return r.post(ctx, input)
	}

	cmd := shell.Command(ctx, r.Command, workDir)
	cmd.Stdin = bytes.NewReader(input)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// post delivers the payload to an HTTP hook. The response body plays the role
// of the command's stdout; a non-2xx status is a hook failure (which blocks
// for pre-tool, prompt, and compact hooks).
func (r Rule) post(ctx context.Context, input []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(input))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHookOutput))
	out := strings.TrimSpace(string(body))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("hook endpoint returned %s", resp.Status)
	}
	return out, nil
}

type payload struct {
	Event     string          `json:"event"`
	ToolName  string          `json:"tool_name,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Result    string          `json:"result,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
	AgentType string          `json:"agent_type,omitempty"`
}

func rawArgs(call tool.ToolCall) json.RawMessage {
	if json.Valid([]byte(call.Args)) {
		return json.RawMessage(call.Args)
	}
	return nil
}
