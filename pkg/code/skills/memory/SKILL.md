---
name: memory
description: Save durable facts to persistent per-project memory using the normal file tools.
when-to-use: When the user shares a durable fact about themselves or their work; when correcting your approach or validating a non-obvious one; when asked to remember or forget something; when you suspect a relevant memory already exists.
---
# Memory

You have a persistent, file-based memory at the path shown in the Memory section of the system prompt (typically `~/.wingman/projects/{cwd}/memory/`). Memory survives across conversations: the index (`MEMORY.md`) is injected into the static prefix of every future session's system prompt, so anything written there is automatically available next time.

You manage memory with the **normal file tools** — `write`, `edit`, `read`, `glob`. The memory directory is an allowed write root, so workspace-relative path rules don't apply: pass the absolute path inside the memory directory.

If the user asks you to remember something, save it. If they ask you to forget something, find and delete it.

## When to save

- After any "remember this" / "from now on" instruction.
- When the user corrects your approach ("no, don't do that") OR validates a non-obvious one ("yeah the bundled PR was the right call here"). Save from both — saving only corrections makes you overly cautious; saving validated calls preserves judgment.
- When you learn durable facts about the user (role, expertise, preferences, what they're focused on).
- When you learn project context that isn't in the code: who is doing what, why, by when; what motivates an in-flight rewrite; what deadline is approaching.
- When the user references an external system worth pointing at later (Linear project, Slack channel, Grafana dashboard, runbook).

## What NOT to save

These exclusions hold even when the user asks you to save them — instead, ask what was *surprising* or *non-obvious* about them and save that.

- Code patterns, conventions, architecture, file paths, project structure — re-derivable by reading the repo.
- Git history, recent diffs, who-changed-what — `git log` / `git blame` are authoritative.
- Debugging recipes — the fix is in the diff; the commit message has the context.
- Anything already in AGENTS.md / CLAUDE.md.
- Ephemeral conversation state — in-progress task details, current scratch context.

## Memory types

- **user** — the user's role, goals, responsibilities, knowledge, stable preferences. Lets you tailor explanations to who they actually are.
- **feedback** — guidance about how to approach work. Lead with the rule, then `**Why:**` (the reason the user gave) and `**How to apply:**` (when this kicks in) so future-you can judge edge cases.
- **project** — in-flight initiatives, decisions, bugs, incidents, deadlines, stakeholders. Convert relative dates to absolute ones ("Thursday" → "2026-03-05") so the memory stays interpretable. Lead with the fact, then `**Why:**` and `**How to apply:**`.
- **reference** — pointers to where information lives in external systems.

## File shape

Each memory is one markdown file with a tiny YAML frontmatter and a body:

```
---
name: feedback_testing
description: integration tests must hit a real database; no mocks
type: feedback
---

Integration tests must hit a real database, not mocks.

**Why:** prior incident where mock/prod divergence masked a broken migration.
**How to apply:** any test under `internal/db/...` or that exercises a query path.
```

Filename is `{name}.md` — lowercase letters, digits, underscore, hyphen. Group semantically: `user_role.md`, `feedback_testing.md`, `project_auth_migration.md`. Cap files at ~25 KB.

## The MEMORY.md index

`MEMORY.md` is a flat list of one-line pointers, no frontmatter:

```
- [feedback_testing](feedback_testing.md) — no DB mocks; real DB only
- [user_role](user_role.md) — senior Go engineer, new to this repo's frontend
```

Keep entries under ~150 chars. **Every memory save or delete must update `MEMORY.md`** — that's what makes the memory show up in the next session's prompt. Both calls can run in parallel in the same turn:

1. `write` the memory file at `{memory_dir}/{name}.md`.
2. `edit` `{memory_dir}/MEMORY.md` to add (or update, or remove) the corresponding `- [name](name.md) — hook` line.

If `MEMORY.md` doesn't exist yet, use `write` to create it with a single `# Memory index` header and your first entry.

## Workflow recipes

**Save a new memory.** Parallel-call:
- `write` to `{memory_dir}/feedback_testing.md` with frontmatter + body.
- `edit` on `{memory_dir}/MEMORY.md` to append `- [feedback_testing](feedback_testing.md) — no DB mocks; real DB only`.

**Update an existing memory.** Use `edit` on `{memory_dir}/{name}.md` for surgical changes. If the hook line in `MEMORY.md` no longer reflects the file, `edit` `MEMORY.md` too.

**Forget a memory.** Use `shell` `rm {memory_dir}/{name}.md`, and `edit` `MEMORY.md` to remove its entry. Or if all you need is to revise the fact, edit the file instead of deleting.

**List what's remembered.** `glob` `*.md` inside the memory dir; or just consult the `MEMORY.md` block already injected at the top of the system prompt.

**Inspect a specific memory.** `read` with the absolute path inside the memory dir.

## Before recommending from memory

A memory that names a function, file, or flag is a claim that it existed *when the memory was written*. Before acting on it: verify the file still exists, grep for the symbol, sanity-check the constraint still holds. Trust what you observe now over what the memory says — and update or remove the stale memory rather than acting on it.

A memory that summarizes repo state is frozen in time. If the user asks about *recent* or *current* state, prefer fresh `git log` / `read` over the snapshot.

## Memory vs. other persistence

- Use **tasks** (not memory) for in-conversation progress tracking.
- Use a **plan** (not memory) to align on approach for the current task.
- Use **memory** only for things that should outlive this conversation.
