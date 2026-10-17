---
name: init
description: Create or refresh AGENTS.md from repository evidence. Use for project guidance and stale agent instructions.
---
# Initialize Project Guidance

Create or update the requested instruction file, defaulting to the root `AGENTS.md`. Preserve useful guidance and remove stale or duplicated instructions. Inspect existing `AGENTS.md`, `CLAUDE.md`, and scoped instructions before deciding what belongs at the root.

## Gather evidence

Find the actual build, test, lint, and development commands in task files, package manifests, CI, and contributor documentation. Include the directory a command runs from, prerequisites that are easy to miss, and a focused test invocation where available. Run a narrow safe check when practical; distinguish a command discovered in configuration from one successfully executed.

Trace the few architecture or ownership boundaries an agent must understand before editing. Identify generated files and their source, compatibility constraints, integration-test requirements, and recurring workflow traps. Check explicit documentation and representative code before treating a pattern as a project rule.

## Write the smallest useful guide

Include only information that changes how an agent should work in this repository. Prefer concrete commands and non-obvious constraints over a stack inventory, directory tour, or generic coding advice. Link maintained documentation instead of copying it. Keep package-specific rules in their existing scope; do not create nested instruction files unless needed by the request.

Resolve contradictions against current evidence while preserving the user's stated conventions. Do not turn one session's workaround into a permanent restriction. Omit temporary task state, branch names, speculative rules, empty sections, and instructions already enforced by tooling.

Reread the result against its sources. Report the file updated, consequential corrections, and commands or assumptions that remain unverified.
