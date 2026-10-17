---
name: architecture
description: Design or assess system architecture, compare technical approaches, or produce an ADR or architecture diagram.
---
# Architecture

Produce a useful decision or an accurate model of the system at the requested level of detail.

Design or analysis target: $ARGUMENTS

Infer whether the user wants an explanation of the existing system, a design proposal, an architecture review, or a saved document. Analysis alone does not request implementation. When design is part of a build request, use the result to continue that work.

## Ground the model

Establish the important user flows, constraints, compatibility requirements, and open decisions. Read entry points, component interfaces, data ownership, deployment configuration, and analogous implementations. Separate observed behavior, documented intent, and assumptions. Ask for missing requirements only when they could change the recommendation.

For a new system without code, work from the supplied requirements and make assumptions explicit. Do not invent load, latency, availability, team structure, or production topology. If estimates matter, show their variables and assumptions.

## Resolve the consequential decisions

Recommend the simplest design that meets the requirements. Work through the dimensions that affect it:

- component responsibilities and dependency direction;
- API and data contracts, ownership, consistency, and versioning;
- synchronous or asynchronous flow, ordering, retries, idempotency, and backpressure;
- failure handling, degraded behavior, resource limits, and observability;
- trust boundaries and sensitive-data handling;
- migration, compatibility, rollout, and rollback.

Each added service, queue, cache, or data copy should justify its operational cost. Identify the likely bottleneck when scale matters. Compare credible alternatives using the constraints that decide between them, and state what new evidence would change the recommendation.

For an existing-system review, identify strengths to preserve and concrete pressure points with code evidence. Separate demonstrated problems from concerns that arise only under future load or ownership changes.

## Deliver

Lead with the recommendation or the system's organizing idea. Explain responsibilities and end-to-end flow, decisive trade-offs, and unresolved questions. Include implementation steps and a verification strategy when proposing a change. Keep depth proportional to the decision.

For diagrams, read [references/diagrams.md](references/diagrams.md). For an ADR, follow the repository's convention; otherwise use context, decision, alternatives, consequences, and validation. Present in chat unless the user asks for a file. When updating a document, reconcile it with current evidence and preserve unrelated useful content.
