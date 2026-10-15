# Model prompts

This package builds the instructions used by Wingman's built-in agent. Choose
a model with `/model`; its matching prompt variant is selected automatically.
Project guidance comes from `AGENTS.md` or `CLAUDE.md`, and reusable workflows
come from skills. See [customization](../../../README.md#customize-your-workflow).

## Working on templates

Model-specific instructions live in `models/<variant>/mode_agent.txt`. Routing
uses the longest normalized model-name prefix, with shared instructions as the
fallback. Shared Plan and unattended templates control those modes.

`BuildBaseInstructions` combines the model and mode instructions.
`BuildSessionContext` adds project instructions, skills, memory, and environment
context. Keep those shared sections in their existing templates when adapting
a model prompt.

Use Wingman's tool names and argument formats. Approval handling, session
continuation, and background commands follow Wingman's runtime behavior.

## Source references

GPT variants adapt the effective Codex model-catalog instructions. The catalog
baseline is commit `be2e129ecd67d900c016eda12d5d193e33d9c3da`. Claude variants
adapt the rendered Claude Code instructions for each model. Gemini adapts
`packages/core/src/prompts/snippets.ts` from gemini-cli at commit
`29315e53d0c2e9b78f301f3c24db235b0f3fa597` (Apache-2.0).

## Check a change

From the repository root:

```bash
go test ./pkg/code/prompt ./pkg/code/agent
```

For model-catalog changes, also run:

```bash
go test ./pkg/model ./pkg/external/codex ./pkg/acp/codex ./pkg/agent
```

These tests check routing, rendering, and integration. Use matched model and
effort runs of the [ACP benchmark](../../../bench/acpflow/README.md) to compare
task behavior or latency.
