# Configuration examples

Copy these examples into your project or personal Wingman configuration.
Replace placeholder package names and remote endpoints before enabling them.

| Example | Destination | Use |
|---------|-------------|-----|
| [MCP servers](mcp.json) | `./mcp.json` or `~/.wingman/mcp.json` | Connect local and remote tools |
| [Database specialist](agents/db-expert.md) | `.wingman/agents/` or `~/.wingman/agents/` | Add a read-only custom agent |
| [Release verifier](agents/release-verifier.md) | `.wingman/agents/` or `~/.wingman/agents/` | Add a verification agent with a model preference |
| [Test skill](skills/run-tests/) | `.wingman/skills/run-tests/` | Run tests through a reusable workflow |
| [Release plugin](plugins/release-tools/) | `.wingman/plugins/release-tools/` or `~/.wingman/plugins/release-tools/` | Add skills, MCP servers, and a hook together |
| [Structured output](non-interactive/README.md) | Run from a repository | Extract project metadata as JSON |
| [Debugger samples](debug/README.md) | Open as a workspace | Try Run and Debug |
| [C project](c/README.md) | Open as a workspace | Try navigation, diagnostics, and debugging |

For project instructions, add an `AGENTS.md` or `CLAUDE.md` with build commands,
conventions, and other guidance to the project root.

## Add the test skill

From the Wingman source checkout:

```sh
mkdir -p .wingman/skills
cp -R examples/skills/run-tests .wingman/skills/
```

Copy the whole directory, including its helper script and reference file.
Invoke `/run-tests` for the repository or `/run-tests ./pkg/skill` for one
package. Use `~/.wingman/skills/` instead for a personal installation.

## Add the release plugin

From the Wingman source checkout:

```sh
mkdir -p .wingman/plugins
cp -R examples/plugins/release-tools .wingman/plugins/
```

Update its example MCP settings before using it. Then invoke
`/release-check v1.2.3` or `/release-tools:release-check v1.2.3`. The qualified
name still reaches this plugin if another skill has the same short name.

The plugin includes a skill, MCP settings, shared resources, and a session
hook. Wingman asks before running a project or plugin hook for the first time
in a session. Use `~/.wingman/plugins/` for a personal installation.
