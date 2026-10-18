---
name: skill-creator
description: Create, evaluate, or improve an Agent Skill's triggers, instructions, and supporting resources.
---
# Skill Creator

Write the guidance a capable agent needs to make better decisions on a recognizable task. A skill should add task-specific knowledge, not repeat generic instructions or prescribe ceremony.

## Understand the workflow

Inspect the existing skill, its callers, and the user's examples. Establish the requests it should handle, nearby requests it should not attract, expected inputs, and successful outputs. Infer these from the conversation before asking for more examples.

For a new skill, choose a discoverable location: `.wingman/skills/<name>/` for this project or `~/.wingman/skills/<name>/` when the user wants it across projects. Respect an explicit destination and any plugin layout. When updating a built-in skill, edit its source rather than the managed `.system` snapshot.

Read [references/skill-format.md](references/skill-format.md) when creating a skill or changing metadata, substitutions, or resource layout. A prose-only revision need not reload it. For a new skill, adapt [assets/SKILL.template.md](assets/SKILL.template.md); omit sections that do not help.

## Write and structure

Make the description a short discovery cue: the capability and the requests it serves. Put distinguishing terms first because catalogs may truncate descriptions. Keep procedure and output details in the body; add exclusions only to prevent likely misrouting. Check neighboring skills for overlap instead of making every description a catchall.

Keep essential decisions and constraints in `SKILL.md`. Move substantial conditional procedures, schemas, or domain details into directly linked references, with instructions for when to read them. Simple skills do not need reference directories. Do not duplicate the same rule across layers.

Use scripts when deterministic execution or repeated transformations justify them. State runtime, dependencies, side effects, and invocation; the host does not install dependencies or create environments automatically. Use assets for material copied into output. For Python helpers, [assets/.gitignore](assets/.gitignore) can exclude local environments and bytecode.

Match prescription to risk. Explain invariants and decision criteria; avoid fixed agent counts, models, scores, or approval rounds without a concrete need. Preserve user scope, authorization, and host rules. Add only guidance that changes decisions; do not repeat the host's tool, delegation, or verification policy. Make costly exploration and evaluation conditional on what they resolve.

Rewrite, consolidate, or delete instructions that do not improve outcomes. Before removing a resource or skill, check its references and consumers and update affected entry points.

## Validate decisions as well as files

Parse the frontmatter, resolve linked resources, and run any new or changed scripts on representative input. Use the format validator described in the reference when available.

Walk through a representative request, a nearby request that should not trigger the skill, and a consequential edge case. Check whether the instructions lead to the right scope, result, and stopping condition. For complex workflows, behavioral trials in a temporary workspace can provide stronger evidence; use independent agents only when delegation is authorized. Compare observable outcomes rather than matching headings or wording.

Report what changed and what was actually validated. Structural checks do not prove better model behavior; improve further from observed failures rather than accumulating speculative rules.
