# Wingman skill format

A skill is a directory whose required entry point is `SKILL.md`. Every file beneath that directory travels with a bundled skill and is copied into its managed system snapshot, so resources are not limited to a fixed set of folder names. Follow the [Agent Skills specification](https://agentskills.io/specification) for portable skills.

## Minimal frontmatter

```yaml
---
name: example-skill
description: Perform a specific workflow. Use when the user asks for its recognizable outcome.
---
```

The directory and `name` must match. Use 1–64 lowercase ASCII letters, digits, and hyphens; do not start or end with a hyphen or use consecutive hyphens. `description` is required, must be at most 1024 characters, and should say both what the skill does and when to use it.

The optional standard fields are `license`, `compatibility`, `metadata`, and `allowed-tools`. Keep `compatibility` at most 500 characters and metadata keys and values as strings. `allowed-tools` is experimental and does not bypass the host's normal tool approval policy.

For a Wingman or Claude Code skill that does not need to remain portable, `arguments` may be a space-separated string or list of positional names, and `argument-hint` may override the hint displayed beside the slash command. A declaration such as `arguments: [component, source, target]` exposes `$component`, `$source`, and `$target` in the body and displays `[component] [source] [target]` when no explicit hint is supplied. Do not use these fields in an Agent Plugin skill: the plugin specification requires its skills to conform to portable Agent Skills frontmatter.

Body substitutions are `$ARGUMENTS`, `$ARGUMENTS[N]`, `$N`, and declared `$name` arguments. `${SKILL_DIR}` and `${PROJECT_DIR}` resolve directories; the Claude-compatible `${CLAUDE_SKILL_DIR}` and `${CLAUDE_PROJECT_DIR}` names are aliases.

Indexed arguments are zero-based. If the body has no argument placeholder, Wingman appends supplied arguments automatically. Escape an argument placeholder with one backslash when teaching its literal syntax in `SKILL.md`. Substitutions apply to the skill body, not supporting files. Do not rely on a substituted argument when a skill is loaded implicitly; use the current request when no invocation arguments are available.

Wingman does not use `agents/openai.yaml`, Claude's `tools`, or model-selection frontmatter to configure skill execution. Do not copy provider-specific metadata or tool names into a skill unless the destination actually supports them.

## Resource layout

```text
example-skill/
├── SKILL.md
├── scripts/       # optional executable helpers
├── references/    # optional selectively loaded guidance
├── assets/        # optional input, copy, or transformation material
├── templates/     # optional output skeletons
└── examples/      # optional representative inputs or expected outputs
```

The names are conventions, not an allowlist. Link required files from `SKILL.md` with relative paths and explain when each should be used. Avoid deep reference chains.

## Script execution

Scripts are invoked through the agent's normal command tool and whatever execution and approval boundary the host provides; a skill must not assume that boundary includes an OS sandbox. The skill host does not infer dependencies or automatically create a Python virtual environment. Give an exact command and prefer self-contained or standard-library helpers. If dependencies are necessary, document a reproducible environment such as `uv run --with ...`, a lockfile-backed project, or explicit venv setup.

Treat scripts as code, and assets/references/templates as data. Never tell the agent to execute a file merely because it is adjacent to `SKILL.md`.

Validate portable skills with `uvx --from skills-ref agentskills validate <skill-directory>` when the reference CLI is available.
