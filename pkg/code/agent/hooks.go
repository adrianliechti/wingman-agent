package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/external"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

// configureHooks preserves registration order: host hooks, invalid-config guard,
// user, workspace and plugin hooks, then session context. Result transforms run
// in their own harness stage after post-tool hooks and their additional context.
func (s *sessionState) configureHooks() {
	a, sessionCfg := s.parent, s.harness.Config
	ws := a.workspace
	globalHooksPath := userHooksConfigPath()
	globalHooks, globalErr := external.Load(globalHooksPath)
	workspaceHooksPath := filepath.Join(ws.RootPath, ".codex", "hooks.json")
	workspaceHooks, workspaceErr := external.Load(workspaceHooksPath)

	// Fail closed: a hook config the user wrote but that no longer parses must
	// not silently disable the guards it configures.
	if err := errors.Join(globalErr, workspaceErr); err != nil {
		message := fmt.Sprintf("hook configuration is invalid; fix or remove it to unblock tools: %v", err)
		sessionCfg.Hooks.PreToolUse = append(sessionCfg.Hooks.PreToolUse,
			func(context.Context, tool.ToolCall) (hook.PreToolUseOutcome, error) {
				return hook.PreToolUseOutcome{Outcome: hook.Outcome{Block: true, Reason: message}}, nil
			},
		)
	}

	s.resources.hooks = external.NewAsync(func(runtime hook.Runtime, parts []string) {
		// A subagent's hook context describes calls the main turn never saw.
		if runtime.AgentID != "" {
			return
		}
		if !s.harness.QueueInput([]harness.Content{{Text: strings.Join(parts, "\n\n"), Hidden: true}}) {
			s.stashHookContext(parts)
		}
	})
	hookOptions := external.BuildOptions{Source: globalHooksPath, Async: s.resources.hooks, MCP: ws.CallMCPTool, SpillDir: ws.ScratchPath}
	hookTrust := attendedTrust{TrustStore: ws.HookTrust(), session: s}

	// Workspace hooks come with the repo, not the user — gate them behind a
	// confirmation so a cloned project cannot run commands unprompted.
	workspaceOptions := hookOptions
	workspaceOptions.Source = workspaceHooksPath
	if rules := workspaceHooks.RuleCount(); rules > 0 {
		fingerprint, _ := workspaceHooks.Fingerprint(ws.RootPath, workspaceHooksPath)
		workspaceOptions.Gate = &external.Gate{
			Confirm:     a.confirm,
			Message:     fmt.Sprintf("Run %d workspace hook rule(s) from %s?\n\nApproval is remembered until these hooks change.", rules, workspaceHooksPath),
			Fingerprint: func() string { return fingerprint },
			Store:       hookTrust,
		}
	}

	sessionCfg.Hooks.Append(globalHooks.BuildWithOptions(ws.RootPath, hookOptions))
	sessionCfg.Hooks.Append(workspaceHooks.BuildWithOptions(ws.RootPath, workspaceOptions))
	for i := range ws.Plugins {
		p := &ws.Plugins[i]
		if p.Hooks == nil || p.Hooks.RuleCount() == 0 {
			continue
		}
		declarations, declarationsErr := p.Hooks.Fingerprint(ws.RootPath, p.Name, p.Root, p.Data)
		pluginOptions := hookOptions
		pluginOptions.Source = "plugin " + p.Name
		pluginOptions.Gate = &external.Gate{
			Confirm: a.confirm,
			Message: fmt.Sprintf("Run %d hook rule(s) from plugin %q at %s?\n\nApproval is remembered until the plugin changes.", p.Hooks.RuleCount(), p.Name, p.Root),
			Fingerprint: func() string {
				content, err := external.ContentHash(p.Root)
				if declarationsErr != nil || err != nil {
					return ""
				}
				return declarations + "/" + content
			},
			Store: hookTrust,
		}
		pluginOptions.Environment = map[string]string{
			"PLUGIN_ROOT":        p.Root,
			"PLUGIN_DATA":        p.Data,
			"CLAUDE_PLUGIN_ROOT": p.Root,
			"CLAUDE_PLUGIN_DATA": p.Data,
		}
		sessionCfg.Hooks.Append(p.Hooks.BuildWithOptions(ws.RootPath, pluginOptions))
	}
	sessionCfg.Hooks.TransformToolResult = append(sessionCfg.Hooks.TransformToolResult,
		truncation.Transform(ws.ScratchPath),
	)

	sessionCfg.Hooks.UserPromptSubmit = append(sessionCfg.Hooks.UserPromptSubmit,
		func(context.Context, string) (hook.Outcome, error) {
			s.sweepFileChanges()
			parts := s.takeHookContext()
			if notice := formatFileChangeNotice(s.takeFileChanges()); notice != "" {
				parts = append(parts, notice)
			}
			return hook.Outcome{AdditionalContext: parts}, nil
		},
	)
}

func userHooksConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "hooks.json")
}

// attendedTrust remembers only approvals a person gave; unattended ones last for the session.
type attendedTrust struct {
	external.TrustStore
	session *sessionState
}

func (t attendedTrust) Trust(fingerprint string) error {
	if t.session.unattended.Load() {
		return nil
	}
	return t.TrustStore.Trust(fingerprint)
}
