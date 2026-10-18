package shell

import "github.com/adrianliechti/wingman-agent/pkg/harness/tool"

// testDialect pins POSIX-syntax cases to the bash classifier; everything else
// uses the platform shell, so Windows runs them against PowerShell.
func testDialect(posix bool) shellDialect {
	if posix {
		return dialectBash
	}
	return platformShellDialect()
}

func ClassifyEffectFor(command string, posix bool) tool.Effect {
	return classifyEffectDialect(map[string]any{"command": command}, testDialect(posix))
}

func IsReadOnlyCommandFor(command string, posix bool) bool {
	return isReadOnlyCommandDialect(command, testDialect(posix))
}
