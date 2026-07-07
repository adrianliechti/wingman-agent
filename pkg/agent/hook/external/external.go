package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
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
	PreToolUse  []Rule `json:"preToolUse,omitempty"`
	PostToolUse []Rule `json:"postToolUse,omitempty"`
}

type Rule struct {
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
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
	}

	return cfg, errors.Join(errs...)
}

func (c *Config) PreHooks(workDir string, gate *Gate) []hook.PreToolUse {
	var hooks []hook.PreToolUse

	for _, rule := range c.PreToolUse {
		if rule.Command == "" {
			continue
		}

		hooks = append(hooks, func(ctx context.Context, call tool.ToolCall) (string, error) {
			if !rule.matches(call.Name) || !gate.Allowed(ctx) {
				return "", nil
			}

			out, err := rule.run(ctx, workDir, buildPayload(call, ""))
			if err != nil {
				if out == "" {
					out = err.Error()
				}
				return fmt.Sprintf("blocked by pre-tool hook (%s): %s", rule.Command, out), nil
			}

			return "", nil
		})
	}

	return hooks
}

func (c *Config) PostHooks(workDir string, gate *Gate) []hook.PostToolUse {
	var hooks []hook.PostToolUse

	for _, rule := range c.PostToolUse {
		if rule.Command == "" {
			continue
		}

		hooks = append(hooks, func(ctx context.Context, call tool.ToolCall, result string) (string, error) {
			if !rule.matches(call.Name) || tool.IsImageResult(result) || !gate.Allowed(ctx) {
				return result, nil
			}

			out, err := rule.run(ctx, workDir, buildPayload(call, result))
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

func (r Rule) run(ctx context.Context, workDir string, input []byte) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := shell.Command(ctx, r.Command, workDir)
	cmd.Stdin = bytes.NewReader(input)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

type payload struct {
	ToolName string          `json:"tool_name"`
	Args     json.RawMessage `json:"args,omitempty"`
	Result   string          `json:"result,omitempty"`
}

func buildPayload(call tool.ToolCall, result string) []byte {
	p := payload{ToolName: call.Name, Result: result}
	if json.Valid([]byte(call.Args)) {
		p.Args = json.RawMessage(call.Args)
	}
	data, _ := json.Marshal(p)
	return data
}
