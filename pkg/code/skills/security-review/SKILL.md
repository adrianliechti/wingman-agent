---
name: security-review
description: Audit code for vulnerabilities, verify supplied security findings, or fix supported findings when requested.
---
# Security Review

Find and explain real security failures using a concrete attacker-to-impact path. Match the requested scope and mode.

Target or findings: $ARGUMENTS

- Audit or scan: inspect the requested code, diff, or repository. Static source review is the default; it does not imply running an external scanner or probing a service.
- Triage supplied findings: read [references/triage.md](references/triage.md) and verify them against the relevant source revision.
- Fix findings: establish the evidence first, then follow [references/remediation.md](references/remediation.md). A request to audit and fix includes both steps without another handoff.

Use the conversation if the argument is empty. Review and triage leave target code unchanged. Execute checks or write report artifacts when requested or within an already authorized remediation workflow; never treat an audit request as permission to probe a live service.

## Establish the security context

Identify the assets, entry points, principals, privilege levels, deployment assumptions, and trust boundaries relevant to the target. Read an existing threat model when available, checking it against current code. A CLI argument, environment variable, internal request, or repository file is not inherently trusted: determine who controls it and whose authority the process exercises.

State material assumptions and review coverage. Start with attacker-controlled entry points and privileged effects, then trace the connecting paths and their protections. Use searches to locate these paths; a pattern match is not a finding. Prioritize consequential trust boundaries rather than giving every source file equal attention. Verify controls in code and configuration rather than inferring safety from a framework name.

## Verify candidates

For each candidate, establish:

1. What the attacker controls and what access or preconditions are required.
2. How that input or state reaches the operation, citing source locations.
3. Which security property fails and the concrete impact.
4. What existing controls apply, and why they do not prevent this path.
5. What evidence would refute the finding or is still missing.

Attempt to refute reachability, attacker control, and impact before accepting the claim. Deduplicate by root cause while retaining affected paths. Confidence scores or agreement between reviewers do not establish exploitability.

Do not exclude vulnerability classes categorically; judge the actual trust boundary and impact. Dangerous-looking syntax, a version number, or absent hardening alone does not demonstrate a vulnerability. Redact secret values in reports.

## Report

Lead with confirmed findings ordered by impact and exploitability. Include a stable identifier, source location, affected revision when known, attacker and preconditions, source-to-effect evidence, existing controls, consequence, and a specific remediation. Use severity labels justified by the impact and access required; do not calculate severity by counting preconditions or matching threat-model labels.

Keep unresolved candidates separate with the missing evidence and next discriminating check. Do not present them as confirmed or silently call them false positives. Finish with coverage and verification limits. If no issue is confirmed, say “No confirmed vulnerabilities found in the reviewed scope.”

Return results in chat unless a saved report is requested. For artifacts, preserve the requested format and identifiers, and include scope, revision, finding status, and evidence so another pass can reproduce the judgment. Reuse supplied report formats where practical; do not require a particular filename or a chain of slash commands.
