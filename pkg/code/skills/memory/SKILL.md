---
name: memory
description: Save, retrieve, update, or forget durable project memory and user preferences.
---
# Memory

Use absolute paths inside the memory directory supplied in the session context and the normal file tools. If no directory is supplied, report that persistence is unavailable. Do not guess a path or substitute a repository file.

## Select and maintain facts

Honor explicit remember, revise, and forget requests. Otherwise retain durable preferences, scoped corrections, confirmed approaches, project constraints, and external pointers likely to help future work. Preserve reasons and exceptions; one decision does not establish a universal preference, and a plan is not evidence of completed work.

Avoid automatically duplicating code, git history, project instructions, or transient progress. If the user explicitly wants a repository fact remembered, include its source and verification context. Never store credentials or secret values.

Use the index to locate relevant files, then read them before relying on or updating them. Verify code or configuration references against current state. Update stale facts and near-duplicates instead of adding competing memories; current user instructions and observed state take precedence.

## File format

Keep one coherent fact per lowercase `{type}_{topic}.md` file. Use `user` for stable user context, `feedback` for working preferences, `project` for decisions or constraints, and `reference` for external pointers.

```markdown
---
description: A specific relevance hint under 150 characters
type: feedback
---

State the fact and its scope.

**Why:** The user's reason, when known.
**How to apply:** Conditions and exceptions.
```

The runtime indexes top-level Markdown files using filenames and descriptions; no separate index file is needed. Keep entries concise, link maintained sources, and convert relative dates when their absolute meaning is known.

Forgetting removes the requested fact, including duplicates, while preserving unrelated content. Confirm what was saved, changed, or removed only after the operation succeeds.
