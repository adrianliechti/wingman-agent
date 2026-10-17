package theme

import "github.com/adrianliechti/wingman-agent/pkg/tui/ansi"

// LogoColors are the wordmark's monochrome gradient stops from top highlight to
// bottom shadow.
var LogoColors = [...]ansi.Color{
	ansi.Hex("#e0e0e0"),
	ansi.Hex("#c8c8c8"),
	ansi.Hex("#b0b0b0"),
	ansi.Hex("#989898"),
	ansi.Hex("#808080"),
	ansi.Hex("#686868"),
}
