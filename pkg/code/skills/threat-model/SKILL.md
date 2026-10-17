---
name: threat-model
description: Model assets, attackers, trust boundaries, and abuse scenarios to prioritize a system's security work.
---
# Threat Model

Explain what could go wrong, who could cause it, and which controls reduce the risk. A threat describes an abuse scenario; a vulnerability is a particular flaw that may enable it.

System or scope: $ARGUMENTS

Use the repository and conversation when no target is given. Analyze without changing implementation. Present the model in chat unless the user asks for a document; use their path or the repository's convention when saving it.

## Reconstruct the system

Read entry points, data flows, identity and authorization checks, storage, external integrations, and deployment configuration. Identify valuable assets and the principals that can read, change, execute, or delete them. Consult existing designs, incidents, and security reports as evidence, checking their currency.

Separate facts visible in code from deployment assumptions and owner intent. Ask for missing information when it changes who is trusted or what impact matters. Do not require a full interview when the repository and request already establish the scope.

## Develop abuse scenarios

For each meaningful boundary, describe an actor's starting access, controlled input or state, path across the boundary, affected asset, and consequence. Consider malicious files or dependencies, cross-tenant access, confused deputies, external tool capabilities, and operational controls where the system actually exposes them.

Use spoofing, tampering, repudiation, disclosure, denial of service, and privilege escalation as prompts for gaps, not mandatory findings. Distinguish existing controls from proposed ones, and identify the evidence that a control covers the path. Do not infer an enforcement boundary merely from a process, folder, or service name.

Rank threats using impact and plausible opportunity, stating assumptions rather than inventing numeric scores. A threat is not accepted risk unless an owner or documented decision accepts it; otherwise call it unresolved or deprioritized with a reason.

## Deliver

Include system context and scope, assets and actors, entry points and trust boundaries, and prioritized threats. Give threats stable IDs, abuse paths, existing controls, residual risks, and concrete mitigations or verification work. A compact data-flow diagram can clarify the boundaries.

Close with material unknowns and the highest-value next investigations. Keep confirmed vulnerabilities distinct from threat scenarios. When refreshing an existing model, preserve stable identifiers and record how new evidence changes the assessment.
