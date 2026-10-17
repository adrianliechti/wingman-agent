---
name: simplify
description: Refactor a requested area or recent changes for clarity and reuse while preserving observable behavior.
---
# Simplify

Make the code easier to understand and maintain without changing its contract.

Scope: $ARGUMENTS

Use the requested files or change. Otherwise inspect the current diff, including staged changes; if it is empty, use files explicitly discussed in the conversation. Do not expand to unrelated code merely to find something to improve.

## Find worthwhile changes

Read the implementation, callers, and relevant tests. Look for:

- duplicated behavior that an existing helper already expresses correctly;
- redundant state, unnecessary layers, or branches that obscure the main flow;
- names and boundaries that hide ownership or invariants;
- repeated work on a demonstrated hot path;
- stale comments or explanations of obvious syntax.

Compare contracts before reusing a helper: similar-looking code may have different validation, ordering, error, or lifetime semantics. A shared abstraction should reduce conceptual complexity, not just line count. Preserve comments that explain constraints, compatibility, or non-obvious decisions.

## Refactor and check

Apply the worthwhile changes directly within scope. Preserve outputs, error behavior, side-effect ordering, concurrency, resource ownership, and public interfaces. A performance change that alters these contracts is a separate behavior change.

Review the resulting diff and run checks that cover the affected behavior. Do not add tests that merely encode the new internal shape. If a real bug is discovered, distinguish its correction from behavior-preserving cleanup and follow the user's scope.

Summarize the simplifications and verification. If no change improves the code, say so and leave it alone.
