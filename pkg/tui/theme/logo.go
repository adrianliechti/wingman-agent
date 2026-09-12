package theme

import "github.com/adrianliechti/wingman-agent/pkg/tui/ansi"

// LogoColors are the wordmark's brand colors, shared by light and dark mode.
// UI text has its own theme palette so readability changes don't recolor it.
var LogoColors = [...]ansi.Color{
	ansi.Hex("#84a0c6"),
	ansi.Hex("#89b8c2"),
	ansi.Hex("#b4be82"),
	ansi.Hex("#e2a478"),
	ansi.Hex("#e27878"),
	ansi.Hex("#a093c7"),
}
