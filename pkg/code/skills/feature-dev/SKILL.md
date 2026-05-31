---
name: feature-dev
description: Explore an existing codebase, design a concrete feature architecture, implement in small phases, and verify with focused tests.
when-to-use: When the user asks to build a non-trivial feature or change and wants the agent to understand local patterns before editing.
arguments: [request]
---
# Feature Development

Deliver a feature by first understanding the existing system, then making a concrete architecture decision, then implementing and verifying in small slices. Use this for changes large enough that direct editing would risk missing project patterns.

`${request}` is the feature or change request. If it is empty, use the user's latest message.

## Phase 1: Explore

Launch a `code-explorer` agent with a self-contained prompt:

- user request;
- likely paths or symbols, if known;
- project guideline files (`AGENTS.md`, `CLAUDE.md`) that apply;
- request for entry points, execution flow, data flow, key abstractions, similar existing features, and essential files.

The explorer must cite file:line references and distinguish facts from inference.

## Phase 2: Architecture

Launch a `code-architect` agent with the request and explorer findings. Ask for one implementation blueprint, not a menu:

- patterns and conventions found;
- files to create or modify;
- component responsibilities and interfaces;
- data flow;
- error handling, persistence, security, performance, and compatibility concerns;
- focused test strategy;
- phased build sequence.

If the blueprint exposes ambiguity that changes behavior or scope, ask the user before editing. Otherwise proceed.

## Phase 3: Implement

Work through the blueprint in small, reviewable phases:

1. Edit the minimum set of existing files first.
2. Add new files only when the existing architecture calls for them.
3. Keep public behavior and APIs stable unless the request explicitly changes them.
4. Follow local style and helper APIs instead of inventing parallel patterns.
5. Leave unrelated refactors for later.

Use `test-engineer` for tests when the behavior is broad, stateful, security-sensitive, or regression-prone. For a narrow change, add focused tests directly.

## Phase 4: Review and verify

After implementation:

1. Launch a `code-reviewer` agent on the diff for bugs, guideline violations, and security regressions.
2. Launch a `code-simplifier` agent if the diff includes new abstractions, repeated logic, or changed hot paths.
3. Run the relevant build, test, lint, or type-check commands using a `verification` agent or directly if the commands are obvious.
4. Fix real issues and rerun the failed checks.

## Phase 5: Report

Summarize:

- what changed;
- key files touched;
- tests/checks run and their result;
- any deliberate non-goals, follow-up risks, or manual verification still needed.

