package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os/exec"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
)

const reviewCommandDescription = "Run an isolated read-only code review on uncommitted, branch, or commit changes"
const reviewCommandHint = "[base-branch | commit | focus instructions]"

const isolatedReviewSystemRubric = `You are Wingman's dedicated Code Reviewer running in an isolated, read-only review pass.

Review the target changes with high precision for concrete, actionable defects, security vulnerabilities, behavioral regressions, broken invariants, race conditions, resource leaks, and missing test coverage on critical paths.

Rules:
- Do NOT modify any files or run mutating commands. Use read-only tools (` + "`read`" + `, ` + "`grep`" + `, ` + "`glob`" + `, ` + "`lsp`" + `, or read-only ` + "`git`" + ` commands) to inspect surrounding context, callers, and contracts before reporting a finding.
- Order findings by severity using priority tags:
  - ` + "`[P0]`" + ` Critical: security vulnerability, data loss/corruption, crash/panic on reachable path, or broken build.
  - ` + "`[P1]`" + ` High: wrong behavior, broken API/contract, race condition, or subtle regression under realistic inputs.
  - ` + "`[P2]`" + ` Medium: edge-case bug, error-handling gap, performance trap, or missing regression test for non-trivial logic.
  - ` + "`[P3]`" + ` Low: minor maintainability or clarity hazard with concrete impact.
- Cite exact ` + "`file_path:line_number`" + ` locations and explain the concrete trigger condition and impact for every finding.
- Avoid stylistic nitpicks, speculative hypotheticals, or repeating what the diff obviously does.
- If no material defect is found, state clearly that no material issues were found and note any residual verification gap.`

// Commands exposes native slash commands supported by the code agent across
// TUI, web (AHP), and ACP clients.
func (a *Agent) Commands(sessionID string) []code.Command {
	if a.session(sessionID) == nil {
		return nil
	}
	return []code.Command{
		{
			Name:        "review",
			Description: reviewCommandDescription,
			InputHint:   reviewCommandHint,
		},
	}
}

func parseReviewSlashInput(input []harness.Content) (args string, rest []harness.Content, ok bool) {
	for i, c := range input {
		if c.Hidden || c.File != nil || strings.TrimSpace(c.Text) == "" {
			continue
		}
		name, matched := skill.ParseSlashCommand(c.Text)
		if !matched || !strings.EqualFold(name, "review") {
			return "", nil, false
		}
		trimmed := strings.TrimSpace(c.Text)
		// Strip leading "/review"
		after := strings.TrimSpace(trimmed[len("/review"):])
		remaining := make([]harness.Content, 0, len(input)-1)
		for j, item := range input {
			if j == i {
				continue
			}
			remaining = append(remaining, item)
		}
		return after, remaining, true
	}
	return "", nil, false
}

const maxInlineReviewDiffBytes = 48 * 1024

func buildReviewTargetContext(ctx context.Context, rootPath, rawArgs string) string {
	var b strings.Builder
	b.WriteString("# Review Request\n\n")
	args := strings.TrimSpace(rawArgs)
	if args != "" {
		fmt.Fprintf(&b, "User review target / focus: %s\n\n", args)
	} else {
		b.WriteString("Target: current working-tree and staged changes (or `HEAD` commit if working tree is clean).\n\n")
	}

	if rootPath == "" {
		return b.String()
	}

	statusOut, statusErr := exec.CommandContext(ctx, "git", "-C", rootPath, "status", "--short").Output()
	fields := strings.Fields(args)
	var diffOut []byte
	var diffLabel string
	targetSelected := false
	if len(fields) == 1 {
		token := fields[0]
		if resolved, err := exec.CommandContext(ctx, "git", "-C", rootPath, "rev-parse", "--verify", "--end-of-options", token+"^{commit}").Output(); err == nil {
			targetSelected = true
			revision := strings.TrimSpace(string(resolved))
			ref, _ := exec.CommandContext(ctx, "git", "-C", rootPath, "rev-parse", "--symbolic-full-name", "--end-of-options", token).Output()
			name := strings.TrimSpace(string(ref))
			if strings.HasPrefix(name, "refs/heads/") || strings.HasPrefix(name, "refs/remotes/") {
				// A base branch denotes this branch's changes since divergence.
				if base, err := exec.CommandContext(ctx, "git", "-C", rootPath, "merge-base", "HEAD", revision).Output(); err == nil {
					diffOut, _ = exec.CommandContext(ctx, "git", "-C", rootPath, "diff", strings.TrimSpace(string(base)), "--").Output()
					diffLabel = fmt.Sprintf("git diff merge-base(HEAD, %s)", token)
				}
			} else {
				// A commit denotes that commit's patch, not every change after it.
				diffOut, _ = exec.CommandContext(ctx, "git", "-C", rootPath, "show", "--format=medium", revision, "--").Output()
				diffLabel = fmt.Sprintf("git show %s", token)
			}
		}
	}

	if !targetSelected {
		if out, err := exec.CommandContext(ctx, "git", "-C", rootPath, "diff", "HEAD", "--").Output(); err == nil && len(out) > 0 {
			diffOut, diffLabel = out, "git diff HEAD"
		} else if out, err := exec.CommandContext(ctx, "git", "-C", rootPath, "diff", "--cached", "--").Output(); err == nil && len(out) > 0 {
			diffOut, diffLabel = out, "git diff --cached"
		} else if statusErr == nil && len(statusOut) == 0 {
			diffOut, _ = exec.CommandContext(ctx, "git", "-C", rootPath, "show", "--format=medium", "HEAD", "--").Output()
			diffLabel = "git show HEAD"
		}
	}
	if len(statusOut) > 0 {
		fmt.Fprintf(&b, "## Working Tree Status (`git status --short`)\n```\n%s\n```\n\n", strings.TrimSpace(string(statusOut)))
	}

	if len(diffOut) > 0 {
		raw := string(diffOut)
		truncated := false
		if len(raw) > maxInlineReviewDiffBytes {
			raw = raw[:maxInlineReviewDiffBytes]
			truncated = true
		}
		fmt.Fprintf(&b, "## Diff Snapshot (`%s`)\n```diff\n%s\n```\n", diffLabel, strings.TrimRight(raw, "\n"))
		if truncated {
			b.WriteString("\n[Diff truncated at 48 KiB; use `read`, `grep`, or read-only `git diff` commands to inspect remaining files.]\n")
		}
	} else {
		b.WriteString("No uncommitted git diff was detected automatically; use read-only tools (`git status`, `git log -n 5`, `read`, `grep`) to inspect the requested scope.\n")
	}

	return b.String()
}

// runIsolatedReview executes `/review` in an ephemeral read-only child agent so
// intermediate file reads and diffs do not bloat the parent session's context,
// then records the user `/review` request and final review report in the parent
// session journal.
func (s *sessionState) runIsolatedReview(ctx context.Context, rawInput []harness.Content, reviewArgs string, extra []harness.Content) (iter.Seq2[harness.Message, error], error) {
	rawInput = harness.CloneContent(rawInput)
	extra = harness.CloneContent(extra)
	return s.harness.RunExclusiveStream(ctx, func(ctx context.Context) (iter.Seq2[harness.Message, error], error) {
		return s.prepareIsolatedReview(ctx, rawInput, reviewArgs, extra)
	})
}

func (s *sessionState) prepareIsolatedReview(ctx context.Context, rawInput []harness.Content, reviewArgs string, extra []harness.Content) (iter.Seq2[harness.Message, error], error) {
	subCfg := s.harness.Config.Derive()
	subCfg.CacheKey = s.id + "-review"
	subCfg.ContextInstructions = nil
	subCfg.ContextSections = nil
	subCfg.PermissionMode = func() string { return "plan" }

	baseTools := s.catalogTools()
	roTools := planModeTools(slices.Clone(baseTools))
	subCfg.Tools = func() []tool.Tool { return slices.Clone(roTools) }
	subCfg.FilterTools = nil

	if opt, ok := s.parent.roleModel(s, "complex"); ok && opt.ID != "" {
		modelID := opt.ID
		subCfg.Model = func() string { return modelID }
		if len(opt.Efforts) > 0 && subCfg.Effort != nil {
			parentEffort := subCfg.Effort
			efforts := slices.Clone(opt.Efforts)
			subCfg.Effort = func() string {
				e := parentEffort()
				if e == "" || slices.Contains(efforts, e) {
					return e
				}
				return efforts[len(efforts)-1]
			}
		}
	}

	sharedCtx := s.subagentContext(roTools)
	subCfg.Instructions = func() string {
		if sharedCtx == "" {
			return isolatedReviewSystemRubric
		}
		return isolatedReviewSystemRubric + "\n\n" + sharedCtx
	}

	reviewer := &harness.Agent{Config: subCfg}
	rootPath := ""
	if s.parent != nil && s.parent.workspace != nil {
		rootPath = s.parent.workspace.RootPath
	}
	promptText := buildReviewTargetContext(ctx, rootPath, reviewArgs)
	reviewInput := make([]harness.Content, 0, 1+len(extra))
	reviewInput = append(reviewInput, harness.Content{Text: promptText})
	reviewInput = append(reviewInput, extra...)

	rt := s.hookRuntime()
	rt.AgentID = s.id + "-review"
	rt.AgentType = "code-reviewer"
	rt.PermissionMode = "plan"
	runCtx := hook.WithRuntime(code.WithSessionID(ctx, s.id), rt)
	childStream, err := reviewer.Send(runCtx, reviewInput)
	if err != nil {
		return nil, err
	}

	userMsg := harness.Message{
		InputID: harness.InputIDFromContext(ctx),
		Role:    harness.RoleUser,
		Content: harness.CloneContent(rawInput),
	}

	if err := s.harness.RecordMessage(userMsg); err != nil {
		_ = reviewer.Close(context.WithoutCancel(ctx), "review setup failed")
		return nil, err
	}
	return func(yield func(harness.Message, error) bool) {
		defer reviewer.Close(context.WithoutCancel(ctx), "review finished")
		consumerOpen := true
		defer func() {
			err := errors.Join(s.harness.RecordUsage(reviewer.UsageSnapshot()), s.parent.Save(s.id))
			if err != nil && consumerOpen {
				yield(harness.Message{}, err)
			}
		}()
		for msg, err := range childStream {
			consumerOpen = yield(msg, err)
			if !consumerOpen || err != nil {
				return
			}
		}
		// Only the final answer belongs in the parent ledger. Streaming text also
		// includes commentary and retries, so take the settled child history.
		messages := reviewer.MessagesSnapshot()
		var report strings.Builder
		for i := len(messages) - 1; i >= 0; i-- {
			m := messages[i]
			if m.Role != harness.RoleAssistant || m.Phase == harness.PhaseCommentary || m.Phase == harness.PhasePartialAnswer {
				continue
			}
			for _, c := range m.Content {
				report.WriteString(c.Text)
			}
			if report.Len() > 0 {
				break
			}
		}
		if text := strings.TrimSpace(report.String()); text != "" {
			if err := s.harness.RecordMessage(harness.Message{
				Role: harness.RoleAssistant, Phase: harness.PhaseFinalAnswer,
				Content: []harness.Content{{Text: text}},
			}); err != nil {
				yield(harness.Message{}, err)
				return
			}
		}
	}, nil
}
