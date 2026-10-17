---
name: code-review
description: Review a diff, commit, or PR for actionable defects and regressions, including focused reviews of contracts or tests.
---
# Code Review

Review the requested change and return every actionable finding. Keep the review read-only unless the user also asks for fixes. Posting comments or approving a PR requires a request to do so.

Review target: $ARGUMENTS

## Resolve the target

Honor the requested scope, including staged-only or unstaged-only review. With no target, inspect `git diff --cached` and `git diff` separately, plus relevant untracked files from `git status --short`. A combined `git diff HEAD` can hide staged edits undone in the worktree. Use `git show :path` when checking staged code that differs from the worktree, and identify which version a finding affects. In a repository without a first commit, inspect the index and worktree directly.

Distinguish a single commit from a branch comparison. For a commit, inspect its patch against the appropriate parent. For a base branch, resolve the comparison ref and use the merge base with `HEAD`; do not compare branch tips directly. Prefer the configured upstream if the local base is behind it, and report unavailable or stale refs. Include worktree changes in a branch review only when requested. For a PR, obtain its actual base, head, description, and full diff through repository tooling; avoid substituting an unrelated local checkout. An explicitly requested draft or previously reviewed PR is still in scope.

Read applicable project instructions and cover the full changed-file list. Inspect large diffs in bounded sections; follow changed paths into callers and tests to establish their contracts. Analyze generated changes through their inputs and relevant output rather than dumping large artifacts into context. If the target is empty, say so. Identify anything not reviewed rather than implying complete coverage.

## Find and challenge defects

Trace observable behavior, particularly error propagation, cleanup, partial writes, cancellation, concurrency, authorization, and old-client or old-data compatibility. Check tests against the behavior they claim to protect. For performance concerns, identify a reachable workload and material consequence.

For each candidate, establish:

- the input, caller, environment, or state that triggers it;
- the changed code responsible and the resulting user or system impact;
- why existing guards, types, tests, or caller contracts do not prevent it;
- that it was introduced by this change and is not the requested behavior.

Try to disprove each candidate before reporting it. Use a focused local check when it can settle the question; do not run unrelated suites just to produce a test result. A missing test alone is a verification gap; a demonstrated defect is a finding. Do not dismiss a real defect merely because CI might catch it.

Deduplicate by root cause. Exclude speculative breakage, unrelated pre-existing bugs, style preferences, and optional refactors. When a project rule materially supports a finding, cite the applicable rule as well as the code. Agent votes and numeric confidence are not substitutes for evidence.

## Report

Lead with findings ordered by impact. For each, give a short title, a precise changed location, and one paragraph explaining the trigger and consequence. Use priorities consistently: P0 for a universal critical blocker, P1 for an urgent defect, P2 for an ordinary actionable defect, and P3 for a low-impact defect worth fixing. Include all qualifying findings without padding the list.

If none qualify, say “No findings.” Then briefly state scope, checks performed, and material verification gaps. Do not equate absence of findings with proof that the change is safe to merge.
