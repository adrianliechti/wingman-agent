---
name: code-review
description: High-precision review of code changes for real bugs, security regressions, and explicit project-guideline violations, with independent verification before reporting.
when-to-use: When the user wants feedback on local changes, a branch diff, or a PR before committing or merging.
arguments: [ref]
---
# Code Review

Review the requested changes and report only findings you have verified are real. The goal is a short, trustworthy list a senior engineer would stand behind, not an exhaustive dump. A false positive costs more trust than a missed nitpick.

## Phase 1: Gather context

1. `git diff` for unstaged and `git diff --cached` for staged changes. If a ref was provided: `git diff ${ref}`.
2. `git status --short` to understand whether staged and unstaged changes are mixed.
3. `git log --oneline -5` to learn commit and change conventions.
4. Read root `AGENTS.md`/`CLAUDE.md` (if present) and any such files in directories the diff touches. These encode project-specific rules the review must check against.

If the diff is empty, say so and stop.

## Phase 2: Find -- launch review agents in parallel

Launch these as read-only agents concurrently in a single message. Give each the full diff, changed-file list, and relevant guideline files. Each agent returns candidate findings only: `file:line`, a one-line claim, and why it flagged it. Tell each: report anything with a plausible problem, but skip pure style nitpicks and pre-existing issues on lines this diff did not touch.

### Agent 1 -- Correctness (`code-reviewer`)
Logic errors (inverted conditions, off-by-one, wrong operator); edge cases (nil/empty, boundaries, concurrency); error handling (swallowed errors, missing cleanup on error paths); resource leaks (files, connections, goroutines); race conditions on shared mutable state; API-contract violations (wrong types, ignored return values).

### Agent 2 -- Quality & consistency (`code-reviewer`)
Does the change follow the patterns in the surrounding code? Naming clarity; functions doing too much; inconsistent abstraction level; dead code (unused imports, unreachable branches, commented-out code); duplication that should reuse an existing helper.

### Agent 3 -- Security (`security`)
Input validation at trust boundaries; injection (SQL, command, XSS, template); secrets or PII in logs/responses; auth/authz gaps introduced by the change. Flag with a concrete data-flow story, not a category name. This is a light pass over the diff only. For a full audit with scan and triage artifacts, point the user to `/vuln-scan` then `/triage`.

### Agent 4 -- Guideline & contract adherence (`code-reviewer`)
For each rule in the gathered `AGENTS.md`/`CLAUDE.md`, check the diff complies. A finding here must quote the specific rule it violates. Skip rules that are guidance for *writing* code rather than reviewable invariants.

### Agent 5 -- Historical and local-context check (`code-explorer`)
Read git history and nearby comments for the changed files. Flag only issues where history or local comments reveal a concrete contract the diff violates.

## Phase 3: Verify -- confirm each candidate before it survives

Collapse duplicates (same `file:line` + same issue). Then, for each remaining candidate, launch a skeptical `code-reviewer` verifier in parallel, one per candidate, with this brief:

> Your default assumption is that this finding is WRONG. Re-read the cited code yourself; don't trust the summary. Confirm the problem is real, is reachable, and is on a line THIS diff introduced or changed. Reject it if: it's pre-existing, a linter/compiler/type-checker would catch it, it's intended behavior, it's a nitpick a senior engineer wouldn't raise, or a guideline finding doesn't map to an actual rule. End with exactly:
> `VERDICT: real | false-positive`
> `CONFIDENCE: 0-100`
> `WHY: <one line citing file:line evidence>`

Keep only findings with `VERDICT: real` and `CONFIDENCE >= 80`. Be willing to drop a plausible-but-unconfirmed finding rather than ship noise.

## Phase 4: Report

Organize survivors by file with `file:line` references. For each:
- **Severity**: error / warning / suggestion
- **What**: the issue (and the rule it violates, for guideline findings)
- **Fix**: the concrete change

End with a one-line verdict: ready to merge, or needs work. If nothing survived verification, say so plainly — "No high-confidence issues found" is a valid and valuable result.
