---
name: feature-dev
description: Implement a substantial feature across components, from exploration through verification. Use for coordinated behavior changes.
---
# Feature Development

Deliver the requested feature end to end. Scale planning and exploration to the uncertainty in the change.

Feature: $ARGUMENTS

Use the conversation when the argument is empty. A request for a plan stops at a plan; a request to build authorizes implementation within its stated scope.

## Establish the behavior

Read project instructions and git status. Trace the nearest existing feature through its entry point, integration boundaries, state changes, and tests. Read the key implementation behind exploration summaries. Once the affected behavior, reusable components, and verification approach are clear, implement; deepen exploration only for a concrete unresolved question.

Turn the request into observable acceptance criteria: what succeeds, what fails, what state changes, and what existing behavior must continue to work. Resolve choices from repository evidence where possible. Ask only when the answer would materially change scope, public behavior, data ownership, or an irreversible migration; continue independent work while awaiting it.

## Choose and implement an approach

Choose one design that fits the existing system. Explain consequential trade-offs and the files or interfaces affected. Compare alternatives only when they could change the decision. For a small feature, this can be a few sentences.

Implement coherent slices that can be exercised independently. Prefer existing abstractions when their contracts fit; introduce a new boundary when it simplifies ownership or behavior. Do not force a tiny patch when a coherent rewrite is needed. Preserve unrelated work and compatibility outside the requested change.

## Verify the result

Check the acceptance criteria with meaningful tests or direct behavior checks using the repository's conventions. Exercise relevant failure paths as well as the successful path. Inspect the final diff for incomplete wiring, stale callers, configuration or migration gaps, and accidental scope growth.

Run the narrowest relevant checks, fix demonstrated issues, and rerun checks invalidated by those fixes. Broaden verification when shared behavior or integration boundaries changed. Update user-facing documentation when the feature changes how the product is used.

Report the resulting behavior, consequential decisions, verification results, and any remaining blocker or material limitation. Distinguish implemented behavior from planned follow-up work.
