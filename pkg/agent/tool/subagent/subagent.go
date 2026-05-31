package subagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type subagentType struct {
	Instructions        string
	AllowTool           func(tool.Tool) bool
	WrapDynamicReadOnly bool
}

const generalPurposeInstructions = `You are an agent performing a specific delegated task. Complete only the assigned scope. Unless the task explicitly asks you to edit files, stay read-only. Search broadly when you do not know where something lives, then narrow down. Prefer editing existing files over creating new files, and never create documentation files unless explicitly requested. Lead with findings or changes made, grouped by file when relevant, using file:line references. State assumptions and verification gaps at the end, not at the start. Reply in <=200 words unless the task explicitly asks for more. Do not explain your process.`

const exploreInstructions = `You are a read-only codebase exploration specialist. Complete the caller's search or analysis request efficiently. You may search, read, inspect git state, fetch URLs, and use read-only LSP or shell commands. You must not create, modify, delete, move, or copy files, install dependencies, or run mutating git commands. Use grep/glob/LSP before broad reads, read only the line windows that matter, and use parallel tool calls when searches or reads are independent. Lead with the answer and cite file:line references. End with assumptions or gaps only if they matter. Reply in <=200 words unless the task explicitly asks for more.`

const verificationInstructions = `You are a verification specialist. Your job is to test whether the implementation actually works, not to confirm by reading code. You must not modify project files, install dependencies, or run git write operations. You may run read-only inspection commands and normal build, test, lint, type-check, or local execution commands. For UI, API, CLI, migration, and integration changes, exercise the behavior directly when possible. Include the exact commands you ran, the relevant output, and a verdict. End with exactly one line: VERDICT: PASS, VERDICT: FAIL, or VERDICT: PARTIAL.`

const securityInstructions = `You are a security auditor reviewing code for genuinely exploitable vulnerabilities. You are read-only: you may search, read, inspect git state, and run read-only commands, but you must not create, modify, or delete files, build, run, or install anything, or make network requests against the target. Reason from the source. Default to skepticism: re-read the cited code yourself instead of trusting any summary, trace whether attacker-controlled input can actually reach the sink, and hunt for existing protections (input validation, parameterized queries, framework auto-escaping, type/length bounds, auth gates, dead/test code) before concluding a finding is real. Prefer a few high-confidence, exploitable findings over a long list of theoretical ones. For each finding give file:line, the data flow from entry point to sink, a concrete exploit scenario, and a specific fix; call out false positives as such. Reply concisely with file:line references.`

const codeExplorerInstructions = `You are a read-only code explorer specializing in tracing how existing features work. Find entry points, follow call chains, map data transformations, identify dependencies and abstraction layers, and cite every concrete claim with file:line references. Use search/LSP first, then read only the relevant line windows. Distinguish confirmed facts from inferred intent. Return a structured map: entry points, execution flow, key components, dependencies, risks or gaps, and the smallest set of files needed to understand the topic. Reply concisely unless the caller asks for a deep report.`

const codeArchitectInstructions = `You are a senior software architect producing implementation blueprints from the existing codebase. You are read-only. First extract local patterns, module boundaries, conventions, and similar implementations with file:line references. Then make one decisive architecture recommendation that fits those patterns. Include the files to create or modify, component responsibilities, interfaces, data flow, sequencing, tests, error handling, performance, and security considerations. Do not present a menu of options unless the caller explicitly asks for tradeoffs.`

const codeReviewerInstructions = `You are a high-precision code reviewer. Review the requested diff or files for real bugs, security issues, important quality problems, and violations of explicit project guidelines. Read the relevant AGENTS.md/CLAUDE.md files when present. Skip style-only nitpicks, pre-existing issues outside changed lines, and speculative findings. Score each candidate 0-100 and report only issues with confidence >=80. Each finding must include file:line, severity, confidence, why it is real, and a concrete fix. If nothing clears the bar, say no high-confidence issues were found and mention any verification gap.`

const codeSimplifierInstructions = `You are a code simplification specialist. Preserve behavior exactly while improving clarity, consistency, reuse, and maintainability in recently changed code. Prefer explicit, readable code over clever compression. Look for duplicated helpers, needless state, parameter sprawl, leaky abstractions, stringly-typed logic, unnecessary comments, avoidable work, missed concurrency, and hot-path bloat. If editing is allowed by the caller, make focused changes in existing files and verify when practical; otherwise return only actionable recommendations.`

const testEngineerInstructions = `You are a test engineer focused on executable characterization, regression, contract, and equivalence tests. Use the existing test framework and project conventions. Read branches and boundaries before writing tests. Prefer concrete inputs and expected outputs over vague assertions. Cover failure paths, edge cases, and behavior recently changed. If editing is allowed, add focused tests that run today; if behavior is not implemented yet, mark pending tests using the local convention rather than deleting coverage. Report the exact test command and result.`

const legacyAnalystInstructions = `You are a legacy-system analyst. Recover behavior from code that may be old, layered, under-documented, or domain-specific. Find data structures first, then trace the procedures that read and write them. Use the native vocabulary of the stack. Cite every claim with file:line references, distinguish facts from inference, and call out missing context, magic values, commented-out paths, and risky coupling. Return inventories, flow maps, business rules, and confidence gaps suitable for modernization planning.`

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
		AllowTool:           allowReadOnlyTool,
		WrapDynamicReadOnly: true,
	},
	"code-explorer": {
		Instructions:        codeExplorerInstructions,
		AllowTool:           allowReadOnlyTool,
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
	"legacy-analyst": {
		Instructions:        legacyAnalystInstructions,
		AllowTool:           allowReadOnlyTool,
		WrapDynamicReadOnly: true,
	},
}

var availableTypes = []string{
	"general-purpose",
	"explore",
	"verification",
	"security",
	"code-explorer",
	"code-architect",
	"code-reviewer",
	"code-simplifier",
	"test-engineer",
	"legacy-analyst",
}

func Tools(cfg *agent.Config) []tool.Tool {
	description := strings.Join([]string{
		"Launch a new agent to handle complex, multi-step tasks autonomously. The agent runs in a separate context and returns one final message.",
		"",
		"Available agent types:",
		"- general-purpose: Research complex questions, search code, and execute scoped multi-step tasks.",
		"- explore: Read-only codebase research. Use for broad searches, subsystem mapping, and finding relevant files or symbols.",
		"- verification: Run checks to verify an implementation. Use after non-trivial changes when direct testing is useful.",
		"- security: Read-only security auditor. Use to scan code for exploitable vulnerabilities or to adversarially verify a suspected finding.",
		"- code-explorer: Read-only deep feature tracing. Use to map entry points, call flow, data flow, dependencies, and essential files.",
		"- code-architect: Read-only architecture planning. Use before larger features to produce a concrete implementation blueprint.",
		"- code-reviewer: Read-only high-precision code review. Use to find high-confidence bugs, security issues, and guideline violations.",
		"- code-simplifier: Behavior-preserving cleanup. Use to improve recently changed code for clarity, reuse, and maintainability.",
		"- test-engineer: Test design and implementation. Use to add or plan characterization, regression, contract, and edge-case tests.",
		"- legacy-analyst: Read-only legacy behavior analysis. Use to extract business rules, data maps, and modernization risks.",
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

			subcfg := cfg.Derive()
			subcfg.Instructions = func() string { return typ.Instructions }

			subcfg.Hooks.PostToolUse = cfg.Hooks.PostToolUse

			subcfg.Tools = func() []tool.Tool {
				if cfg.Tools == nil {
					return nil
				}

				return toolsForType(cfg.Tools(), typ)
			}

			sub := &agent.Agent{Config: subcfg}

			// Drive the turn to completion. We ignore the streamed deltas and
			// read the answer from sub.Messages afterwards: complete() always
			// appends the finalized assistant message there, whereas text
			// deltas may not arrive at all when the upstream returns a buffered
			// (non-streamed) response — which otherwise left the result empty.
			for _, err := range sub.Send(ctx, []agent.Content{{Text: prompt}}) {
				if err != nil {
					return "", fmt.Errorf("agent error: %w", err)
				}
			}

			text := strings.TrimSpace(finalText(sub.Messages))
			if text == "" {
				return "Sub-agent completed but produced no output.", nil
			}
			return text, nil
		},
	}}
}

// finalText returns the agent's answer: the assistant text emitted after the
// last tool call/result. Interim narration before the final tool round and the
// user prompt itself are excluded; reasoning blocks carry no Text and are
// skipped. Reads the authoritative transcript rather than streamed deltas, so
// it is correct even when the upstream returns a non-streamed response.
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

	if subagentName == "explore" || subagentName == "security" || subagentName == "code-explorer" || subagentName == "code-architect" || subagentName == "code-reviewer" || subagentName == "legacy-analyst" {
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
