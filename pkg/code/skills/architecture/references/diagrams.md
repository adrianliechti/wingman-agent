# Architecture diagrams

Choose a view that answers the user's question. One focused diagram is usually more useful than a complete hierarchy.

- Context: people and external systems, when evidenced.
- Containers: independently runnable or deployable units, data stores, and their protocols. A package or directory alone is not a container.
- Components: responsibilities inside one selected container.
- Sequence or dynamic view: a particular request, state transition, or failure path.
- Deployment: runtime placement supported by deployment configuration.

Trace elements and connections to source or configuration before drawing them. Label inferred relationships and unknown deployment details. An imported client does not prove that a service is deployed or reachable in production.

Use Mermaid when supported by the destination. A flowchart or sequence diagram often suffices; use C4 syntax when the renderer supports it and the distinction helps. Use stable short identifiers, action labels on relationships, and protocols when known. Direct dependency arrows from the initiator to its dependency; label return or event flows explicitly.

Keep each view readable. Split unrelated flows instead of adding dozens of nodes. Follow the diagram with the key evidence references and material unknowns. Do not add an architecture critique unless it helps answer the request.

If saving a diagram, use the requested path or the repository's documentation layout. Check syntax with an available renderer when practical; otherwise state that rendering was not verified.
