package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
)

const defaultTimeout = 30

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

func (c *Config) PreHooks(workDir string) []hook.PreToolUse {
	var hooks []hook.PreToolUse

	for _, rule := range c.PreToolUse {
		if rule.Command == "" {
			continue
		}

		hooks = append(hooks, func(ctx context.Context, call tool.ToolCall) (string, error) {
			if !rule.matches(call.Name) {
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

func (c *Config) PostHooks(workDir string) []hook.PostToolUse {
	var hooks []hook.PostToolUse

	for _, rule := range c.PostToolUse {
		if rule.Command == "" {
			continue
		}

		hooks = append(hooks, func(ctx context.Context, call tool.ToolCall, result string) (string, error) {
			if !rule.matches(call.Name) || strings.HasPrefix(result, "data:image/") {
				return result, nil
			}

			out, err := rule.run(ctx, workDir, buildPayload(call, result))
			if err != nil || out == "" {
				return result, nil
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
