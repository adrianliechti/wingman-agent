---
name: commit
description: Create a scoped Git commit, checkpoint local work, or draft a commit message when requested.
---
# Commit

Message or scope hint: $ARGUMENTS

If the user asks only for a commit message, return the message without staging or committing.

## Establish exactly what belongs

Inspect git status, staged and unstaged diffs, relevant untracked files, and recent commit subjects. Use the request and conversation to identify the intended change. If the user asks to commit staged changes, the index defines the scope; do not add unstaged work.

Preserve unrelated work, including pre-existing staged changes. A normal commit includes the entire index: staging only your paths does not isolate it. If unrelated staged content exists, use a path- or hunk-scoped approach only when you can preserve both the index and worktree correctly; otherwise ask about the ambiguity. Never reset, stash, or sweep unrelated changes into the commit as a convenience.

`git commit --only <path>` takes the worktree version and can include unstaged hunks. Do not use it as a shortcut for committing only staged content.

Check the intended content for accidental local files, secrets, generated output, and debug changes. Review meaningful existing verification results; run required checks that have not covered the final change.

## Create and verify

Stage explicit paths or selected hunks. Review the exact content that will be committed, not just a diff summary. If there is no intended change, report that instead of creating an empty commit.

Follow the repository's message conventions and describe the change's purpose. Create a new commit unless the user explicitly requested amendment. Do not bypass hooks. If a hook changes files or fails, inspect the result, fix in-scope issues, and determine whether a commit was created before retrying. Avoid an automatic retry loop on an unchanged failure.

Inspect the resulting commit and git status. Report the hash, subject, and verification outcome, mentioning remaining local work when relevant. Pushing is a separate action unless already requested.
