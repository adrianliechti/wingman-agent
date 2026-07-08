package subagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type subagentType struct {
	Instructions        string
	AllowTool           func(tool.Tool) bool
	WrapDynamicReadOnly bool
}

const generalPurposeInstructions = `You are an agent performing a specific delegated task. Complete only the assigned scope. Unless the task explicitly asks you to edit files, stay read-only. Search broadly when you do not know where something lives, then narrow down. Prefer editing existing files over creating new files, and never create documentation files unless explicitly requested. Lead with findings or changes made, grouped by file when relevant, using file:line references. State assumptions and verification gaps at the end, not at the start. Reply in <=200 words unless the task explicitly asks for more. Do not explain your process.`

const exploreInstructions = `You are a read-only codebase research specialist covering broad searches, feature tracing, and analysis of unfamiliar or legacy code. You may search, read, inspect git state, fetch URLs, and use read-only LSP or shell commands. You must not create, modify, delete, move, or copy files, install dependencies, or run mutating git commands. Use grep/glob/LSP before broad reads, read only the line windows that matter, and use parallel tool calls when searches or reads are independent. When tracing how a feature works, find the entry points, follow the call chain, and map the data flow and key components. When analyzing legacy or under-documented code, find the data structures first, then trace the procedures that read and write them, and call out magic values and risky coupling. Cite every concrete claim with file:line references and distinguish confirmed facts from inference. Lead with the answer; end with assumptions or gaps only if they matter. Reply in <=200 words unless the task explicitly asks for more.`

const verificationInstructions = `You are a verification specialist. Your job is to test whether the implementation actually works, not to confirm by reading code. You must not modify project files, install dependencies, or run git write operations. You may run read-only inspection commands and normal build, test, lint, type-check, or local execution commands. For UI, API, CLI, migration, and integration changes, exercise the behavior directly when possible. Include the exact commands you ran, the relevant output, and a verdict. End with exactly one line: VERDICT: PASS, VERDICT: FAIL, or VERDICT: PARTIAL.`

const securityInstructions = `You are a security auditor reviewing code for genuinely exploitable vulnerabilities. You are read-only: you may search, read, inspect git state, and run read-only commands, but you must not create, modify, or delete files, build, run, or install anything, or make network requests against the target. Reason from the source. Default to skepticism: re-read the cited code yourself instead of trusting any summary, trace whether attacker-controlled input can actually reach the sink, and hunt for existing protections (input validation, parameterized queries, framework auto-escaping, type/length bounds, auth gates, dead/test code) before concluding a finding is real. Prefer a few high-confidence, exploitable findings over a long list of theoretical ones. For each finding give file:line, the data flow from entry point to sink, a concrete exploit scenario, and a specific fix; call out false positives as such. Reply concisely with file:line references.`

const codeArchitectInstructions = `You are a senior software architect producing implementation blueprints from the existing codebase. You are read-only. First extract local patterns, module boundaries, conventions, and similar implementations with file:line references. Then make one decisive architecture recommendation that fits those patterns. Include the files to create or modify, component responsibilities, interfaces, data flow, sequencing, tests, error handling, performance, and security considerations. Do not present a menu of options unless the caller explicitly asks for tradeoffs.`

const codeReviewerInstructions = `You are a high-precision code reviewer. Review the requested diff or files for real bugs, security issues, important quality problems, and violations of explicit project guidelines. Read the relevant AGENTS.md/CLAUDE.md files when present. Skip style-only nitpicks, pre-existing issues outside changed lines, and speculative findings. Score each candidate 0-100 and report only issues with confidence >=80. Each finding must include file:line, severity, confidence, why it is real, and a concrete fix. If nothing clears the bar, say no high-confidence issues were found and mention any verification gap.`

const codeSimplifierInstructions = `You are a code simplification specialist. Preserve behavior exactly while improving clarity, consistency, reuse, and maintainability in recently changed code. Prefer explicit, readable code over clever compression. Look for duplicated helpers, needless state, parameter sprawl, leaky abstractions, stringly-typed logic, unnecessary comments, avoidable work, missed concurrency, and hot-path bloat. If editing is allowed by the caller, make focused changes in existing files and verify when practical; otherwise return only actionable recommendations.`

const testEngineerInstructions = `You are a test engineer focused on executable characterization, regression, contract, and equivalence tests. Use the existing test framework and project conventions. Read branches and boundaries before writing tests. Prefer concrete inputs and expected outputs over vague assertions. Cover failure paths, edge cases, and behavior recently changed. If editing is allowed, add focused tests that run today; if behavior is not implemented yet, mark pending tests using the local convention rather than deleting coverage. Report the exact test command and result.`

var subagentTypes = map[string]subagentType{
	"general-purpose": {
		Instructions: generalPurposeInstructions,
		AllowTool:    allowNonAgentTool,
	},
	"explore": {
		Instructions:        exploreInstructions,
		AllowTool:           allowReadOnlyTool,
		WrapDynamicReadOnly: true,
	},
	"verification": {
		Instructions: verificationInstructions,
		AllowTool:    allowVerificationTool,
	},
	"security": {
		Instructions:        securityInstructions,
		AllowTool:           allowStaticSecurityTool,
		WrapDynamicReadOnly: true,
	},
	"code-architect": {
		Instructions:        codeArchitectInstructions,
		AllowTool:           allowReadOnlyTool,
		WrapDynamicReadOnly: true,
	},
	"code-reviewer": {
		Instructions:        codeReviewerInstructions,
		AllowTool:           allowReadOnlyTool,
		WrapDynamicReadOnly: true,
	},
	"code-simplifier": {
		Instructions: codeSimplifierInstructions,
		AllowTool:    allowNonAgentTool,
	},
	"test-engineer": {
		Instructions: testEngineerInstructions,
		AllowTool:    allowNonAgentTool,
	},
}

var availableTypes = []string{
	"general-purpose",
	"explore",
	"verification",
	"security",
	"code-architect",
	"code-reviewer",
	"code-simplifier",
	"test-engineer",
}

func Tools(cfg *agent.Config, sharedContext func() string) []tool.Tool {
	description := strings.Join([]string{
		"Launch a new agent to handle complex, multi-step tasks autonomously. The agent runs in a separate context and returns one final message.",
		"",
		"Available agent types:",
		"- general-purpose: Research complex questions, search code, and execute scoped multi-step tasks.",
		"- explore: Read-only codebase research. Use for broad searches, subsystem mapping, feature tracing, and legacy-code analysis.",
		"- verification: Run checks to verify an implementation. Use after non-trivial changes when direct testing is useful.",
		"- security: Read-only security auditor. Use to scan code for exploitable vulnerabilities or to adversarially verify a suspected finding.",
		"- code-architect: Read-only architecture planning. Use before larger features to produce a concrete implementation blueprint.",
		"- code-reviewer: Read-only high-precision code review. Use to find high-confidence bugs, security issues, and guideline violations.",
		"- code-simplifier: Behavior-preserving cleanup. Use to improve recently changed code for clarity, reuse, and maintainability.",
		"- test-engineer: Test design and implementation. Use to add or plan characterization, regression, contract, and edge-case tests.",
		"",
		"When to use:",
		"- Open-ended searches where you are not confident that one or two direct tool calls will find the answer.",
		"- Independent research or verification whose intermediate tool output would clutter your context.",
		"- Scoped implementation or investigation work that can proceed without blocking your next step.",
		"",
		"When NOT to use:",
		"- Reading a specific known file path: use `read` instead.",
		"- Finding files by a known pattern: use `glob` instead.",
		"- Searching for a known symbol or string: use `grep` or LSP instead.",
		"- Synthesis across multiple results: do the synthesis yourself after agents return.",
		"",
		"Usage notes:",
		"- Provide a self-contained prompt; the agent does not have your conversation history.",
		"- Include relevant paths, symbols, constraints, allowed edit scope, and expected output shape.",
		"- The `agent_type` parameter is required; choose the narrowest fitting agent type.",
		"- Agent outputs should generally be trusted; verify only when the task requires direct proof.",
	}, "\n")

	return []tool.Tool{{
		Name:        "agent",
		Description: description,
		Effect:      classifyEffect,
		Timeout:     20 * time.Minute,

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"description": map[string]any{"type": "string", "description": "Short 3-5 word label for the UI (for example, `Audit auth middleware`)."},
				"prompt":      map[string]any{"type": "string", "description": "Self-contained task briefing. Include goal, relevant paths/symbols, allowed edit scope, what is out of scope, and the expected report shape."},
				"agent_type": map[string]any{
					"type":        "string",
					"description": "Agent type to use. Must be one of the available agent types.",
					"enum":        availableTypes,
				},
			},

			"required":             []string{"description", "prompt", "agent_type"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			description, ok := args["description"].(string)

			if !ok || strings.TrimSpace(description) == "" {
				return "", fmt.Errorf("description is required")
			}

			prompt, ok := args["prompt"].(string)

			if !ok || strings.TrimSpace(prompt) == "" {
				return "", fmt.Errorf("prompt is required")
			}

			subagentName, ok := args["agent_type"].(string)
			if !ok || strings.TrimSpace(subagentName) == "" {
				return "", fmt.Errorf("agent_type is required")
			}
			subagentName = strings.ToLower(strings.TrimSpace(subagentName))

			typ, ok := subagentTypes[subagentName]
			if !ok {
				return "", fmt.Errorf("unknown agent_type %q (available: %s)", subagentName, strings.Join(availableTypes, ", "))
			}

			instructions := typ.Instructions
			if sharedContext != nil {
				if c := strings.TrimSpace(sharedContext()); c != "" {
					instructions += "\n\n" + c
				}
			}

			subcfg := cfg.Derive()
			subcfg.Instructions = func() string { return instructions }

			subcfg.Tools = func() []tool.Tool {
				if cfg.Tools == nil {
					return nil
				}

				return toolsForType(cfg.Tools(), typ)
			}

			sub := &agent.Agent{Config: subcfg}

			var runErr error
			for _, err := range sub.Send(ctx, []agent.Content{{Text: prompt}}) {
				if err != nil {
					runErr = err
					break
				}
			}

			text := strings.TrimSpace(finalText(sub.Messages))

			if runErr != nil {
				if text == "" {
					return "", fmt.Errorf("agent error: %w", runErr)
				}
				return fmt.Sprintf("Agent aborted before finishing (%v). Last output before the abort — treat as incomplete:\n\n%s", runErr, text), nil
			}

			if text == "" {
				return "Sub-agent completed but produced no output.", nil
			}
			return text, nil
		},
	}}
}

func finalText(messages []agent.Message) string {
	lastTool := -1
	for i, m := range messages {
		for _, c := range m.Content {
			if c.ToolCall != nil || c.ToolResult != nil {
				lastTool = i
			}
		}
	}

	var b strings.Builder
	for _, m := range messages[lastTool+1:] {
		if m.Role != agent.RoleAssistant {
			continue
		}
		for _, c := range m.Content {
			if c.Text != "" {
				b.WriteString(c.Text)
			}
		}
	}

	return b.String()
}

func classifyEffect(args map[string]any) tool.Effect {
	if args == nil {
		return tool.EffectDynamic
	}

	subagentName, ok := args["agent_type"].(string)
	if !ok || strings.TrimSpace(subagentName) == "" {
		return tool.EffectDynamic
	}
	subagentName = strings.ToLower(strings.TrimSpace(subagentName))

	switch subagentName {
	case "explore", "security", "code-architect", "code-reviewer":
		return tool.EffectReadOnly
	}

	return tool.EffectMutates
}

func toolsForType(tools []tool.Tool, typ subagentType) []tool.Tool {
	filtered := make([]tool.Tool, 0, len(tools))

	for _, t := range tools {
		if !typ.AllowTool(t) {
			continue
		}

		if typ.WrapDynamicReadOnly && t.Effect != nil && t.Effect(nil) == tool.EffectDynamic {
			t = readOnlyDynamicTool(t)
		}

		filtered = append(filtered, t)
	}

	return filtered
}

func readOnlyDynamicTool(t tool.Tool) tool.Tool {
	originalExecute := t.Execute
	originalEffect := t.Effect

	t.Execute = func(ctx context.Context, args map[string]any) (string, error) {
		if originalEffect(args) != tool.EffectReadOnly {
			return "", fmt.Errorf("read-only subagent only allows read-only %s calls", t.Name)
		}
		if originalExecute == nil {
			return "", fmt.Errorf("tool %s has no executor", t.Name)
		}
		return originalExecute(ctx, args)
	}

	return t
}

func allowNonAgentTool(t tool.Tool) bool {
	return t.Name != "agent" && !t.Hidden
}

func allowReadOnlyTool(t tool.Tool) bool {
	if t.Name == "ask_user" {
		return false
	}

	if !allowNonAgentTool(t) || t.Effect == nil {
		return false
	}

	switch t.Effect(nil) {
	case tool.EffectReadOnly:
		return true
	case tool.EffectDynamic:
		return true
	default:
		return false
	}
}

func allowStaticSecurityTool(t tool.Tool) bool {
	switch t.Name {
	case "web_fetch", "web_search":
		return false
	default:
		return allowReadOnlyTool(t)
	}
}

func allowVerificationTool(t tool.Tool) bool {
	if !allowNonAgentTool(t) {
		return false
	}

	switch t.Name {
	case "write", "edit", "ask_user":
		return false
	case "shell":
		return true
	default:
		return t.Effect != nil && t.Effect(nil) == tool.EffectReadOnly
	}
}
