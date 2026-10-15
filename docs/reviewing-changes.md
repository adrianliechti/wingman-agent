# Reviewing changes

Use `/diff` in the terminal UI or the browser's changes view to inspect working
tree changes. In the browser, expand **Review changes** below a built-in agent
turn to see its tracked edits and validation commands.

## Check the result

The turn review shows file diffs, recorded validation commands, and their exit
codes. Checks that ran before the latest edits are marked outdated. Run the
relevant checks again after changing the code.

A validation result records a command's outcome. Review the diff and choose
checks that cover the behavior you asked to change.

For Git repositories, stage the intended changes and use the sparkle button by
the commit box to draft a commit message. Review the message before committing.

## Undo tracked edits

Choose **Undo tracked edits** on a turn to restore files to their pre-turn
contents, including uncommitted changes that existed before the turn.

Undo checks all affected files first. If a file has changed again or the edit
history is incomplete, it refuses the operation so you can resolve the conflict.
If an interrupted rollback leaves a backup, the error shows its location.

Turn review covers the built-in edit tool and synchronous child agents. Shell
commands, external tools, detached tasks, and native agents can change files
outside that record. Use Git diffs to inspect the complete working tree. Older
sessions without edit records cannot offer turn undo.

## Set task limits

Limit a built-in agent turn with environment variables:

```bash
export WINGMAN_TASK_MAX_TOKENS=200000
export WINGMAN_TASK_TIMEOUT=20m
wingman
```

Zero or unset disables the corresponding limit. Token usage counts input plus
output, and an in-flight response can exceed the remaining allowance. Inline
helpers share the turn's limit; detached tasks have separate limits. Native
agents use their own limits.

See [sessions and recovery](sessions.md) for stopping work, paused queues, and
recovering unsent input.
