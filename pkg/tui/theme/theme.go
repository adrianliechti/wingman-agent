package theme

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

var Default Theme

func Auto() {
	SetDark()

	if isLightBackground() {
		SetLight()
	}
}

type Theme struct {
	IsLight    bool
	Background ansi.Color
	Foreground ansi.Color
	Surface    ansi.Color // Passive panels, prompt echoes, and attachment chips.
	Selection  ansi.Color
	Border     ansi.Color
	Cursor     ansi.Color
	Black      ansi.Color
	Red        ansi.Color
	Green      ansi.Color
	Yellow     ansi.Color
	Blue       ansi.Color
	Magenta    ansi.Color
	Cyan       ansi.Color
	White      ansi.Color
	BrBlack    ansi.Color
	BrRed      ansi.Color
	BrGreen    ansi.Color
	BrYellow   ansi.Color
	BrBlue     ansi.Color
	BrMagenta  ansi.Color
	BrCyan     ansi.Color
	BrWhite    ansi.Color
}

// Signature identifies every palette value so render caches can detect when
// the terminal-aware palette has been reinitialized.
func (t Theme) Signature() string {
	var signature strings.Builder
	fmt.Fprintf(&signature, "%t", t.IsLight)
	for _, color := range []ansi.Color{
		t.Background, t.Foreground, t.Surface, t.Selection, t.Border, t.Cursor,
		t.Black, t.Red, t.Green, t.Yellow, t.Blue, t.Magenta, t.Cyan, t.White,
		t.BrBlack, t.BrRed, t.BrGreen, t.BrYellow, t.BrBlue, t.BrMagenta, t.BrCyan, t.BrWhite,
	} {
		fmt.Fprintf(&signature, ":%06x", color.Hex())
	}
	return signature.String()
}

func SetDark() {
	Default = Theme{
		IsLight:    false,
		Background: ansi.Hex("#161821"),
		Foreground: ansi.Hex("#c6c8d1"),
		Surface:    ansi.Hex("#2b2d36"),
		Selection:  ansi.Hex("#272c42"),
		Border:     ansi.Hex("#6b7089"),
		Cursor:     ansi.Hex("#c6c8d1"),
		Black:      ansi.Hex("#1e2132"),
		Red:        ansi.Hex("#e27878"),
		Green:      ansi.Hex("#b4be82"),
		Yellow:     ansi.Hex("#e2a478"),
		Blue:       ansi.Hex("#84a0c6"),
		Magenta:    ansi.Hex("#a093c7"),
		Cyan:       ansi.Hex("#89b8c2"),
		White:      ansi.Hex("#c6c8d1"),
		BrBlack:    ansi.Hex("#6b7089"),
		BrRed:      ansi.Hex("#e98989"),
		BrGreen:    ansi.Hex("#c0ca8e"),
		BrYellow:   ansi.Hex("#e9b189"),
		BrBlue:     ansi.Hex("#91acd1"),
		BrMagenta:  ansi.Hex("#ada0d3"),
		BrCyan:     ansi.Hex("#95c4ce"),
		BrWhite:    ansi.Hex("#d2d4de"),
	}
}

func SetLight() {
	// Pale neutral surfaces and darker accents keep text readable in both
	// truecolor terminals and their coarser 256-color fallbacks.
	Default = Theme{
		IsLight:    true,
		Background: ansi.Hex("#ffffff"),
		Foreground: ansi.Hex("#303642"),
		Surface:    ansi.Hex("#f2f4f7"),
		Selection:  ansi.Hex("#dee8f3"),
		Border:     ansi.Hex("#b3bbc7"),
		Cursor:     ansi.Hex("#303642"),
		Black:      ansi.Hex("#dfe3e8"),
		Red:        ansi.Hex("#c41c2b"),
		Green:      ansi.Hex("#16702c"),
		Yellow:     ansi.Hex("#8a5d00"),
		Blue:       ansi.Hex("#075ec5"),
		Magenta:    ansi.Hex("#793ed0"),
		Cyan:       ansi.Hex("#006b9e"),
		White:      ansi.Hex("#303642"),
		BrBlack:    ansi.Hex("#59636f"),
		BrRed:      ansi.Hex("#b11927"),
		BrGreen:    ansi.Hex("#086428"),
		BrYellow:   ansi.Hex("#795100"),
		BrBlue:     ansi.Hex("#0052b0"),
		BrMagenta:  ansi.Hex("#6b32bd"),
		BrCyan:     ansi.Hex("#005e8f"),
		BrWhite:    ansi.Hex("#202631"),
	}
}
