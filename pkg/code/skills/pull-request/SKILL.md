---
name: pull-request
description: Prepare PR text, open a pull request, or update an existing PR within the requested publication scope.
---
# Pull Request

Request or title hint: $ARGUMENTS

Choose the action from the request:

- **Prepare text:** inspect the relevant change and return a title or description. Skip branch creation, commits, pushes, and hosted edits.
- **Update PR text:** inspect the PR and its diff, then update the requested fields. Unpublished local work is outside this operation.
- **Open a PR or publish new code:** prepare the intended branch and publish it. An explicitly requested draft PR is a hosted action too.

## Describe the change

Identify the actual base and head using the existing PR or repository configuration. For local work, inspect status, the commit list, and the complete merge-base diff. Include staged, unstaged, and relevant untracked changes when the request covers uncommitted work. Use individual commit patches when needed to explain intent or check what the history would publish. Do not substitute the latest commit or an unrelated checkout for the branch change.

Follow the repository's PR template. Lead with the problem and resulting behavior, then relevant verification and risks. Keep simple changes short. Describe the final implementation; preserve useful issue references, images, and migration notes while replacing stale text. Use existing check results that still apply. A text-only edit does not require rerunning the project's tests.

## Publish the branch when requested

Confirm the repository, remote, base, and intended commits. Separate unrelated local and staged work; inspect the whole index before committing. Create a topic branch when needed, including when intended commits already exist on the default branch. Preserve history, commit conventions, and hooks.

Inspect for incomplete work, sensitive content in files or history, accidental changes, conflicts, and compatibility implications. Run missing required checks for the final change. Explain failures or unavailable checks and represent draft status honestly; do not claim readiness from incomplete evidence.

Push only when publishing commits is needed, without force. Create a PR when requested and none exists; otherwise update the intended existing PR. Rewriting history, merging, or approving requires a separate request.

For hosted edits, use structured arguments or a body file for multiline text. After an ambiguous failure, inspect remote state before retrying to avoid duplicate PRs. Verify the resulting base, head, and edited fields. Return the PR link and relevant checks, or the prepared text for a local drafting request.
