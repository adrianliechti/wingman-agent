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
		Background: ansi.Hex("#141414"),
		Foreground: ansi.Hex("#eeeeee"),
		Surface:    ansi.Hex("#2a2a2a"),
		Selection:  ansi.Hex("#2a2a2a"),
		Border:     ansi.Hex("#424242"),
		Cursor:     ansi.Hex("#eeeeee"),
		Black:      ansi.Hex("#1e1e1e"),
		Red:        ansi.Hex("#f4878f"),
		Green:      ansi.Hex("#7fd88f"),
		Yellow:     ansi.Hex("#e5c07b"),
		Blue:       ansi.Hex("#7ab0ff"),
		Magenta:    ansi.Hex("#b89bf2"),
		Cyan:       ansi.Hex("#6ad0dc"),
		White:      ansi.Hex("#eeeeee"),
		BrBlack:    ansi.Hex("#8e8e8e"),
		BrRed:      ansi.Hex("#fd969d"),
		BrGreen:    ansi.Hex("#96f7a7"),
		BrYellow:   ansi.Hex("#f5a742"),
		BrBlue:     ansi.Hex("#91c0ff"),
		BrMagenta:  ansi.Hex("#c8b0f8"),
		BrCyan:     ansi.Hex("#80dce7"),
		BrWhite:    ansi.Hex("#ffffff"),
	}
	LogoColors = [...]ansi.Color{
		ansi.Hex("#e0e0e0"),
		ansi.Hex("#c8c8c8"),
		ansi.Hex("#b0b0b0"),
		ansi.Hex("#989898"),
		ansi.Hex("#808080"),
		ansi.Hex("#686868"),
	}
}

func SetLight() {
	// Pale neutral surfaces and darker accents keep text readable in both
	// truecolor terminals and their coarser 256-color fallbacks.
	Default = Theme{
		IsLight:    true,
		Background: ansi.Hex("#ffffff"),
		Foreground: ansi.Hex("#262b33"),
		Surface:    ansi.Hex("#e8eaee"),
		Selection:  ansi.Hex("#e8eaee"),
		Border:     ansi.Hex("#adb6c2"),
		Cursor:     ansi.Hex("#262b33"),
		Black:      ansi.Hex("#d8dde3"),
		Red:        ansi.Hex("#c41c2b"),
		Green:      ansi.Hex("#16702c"),
		Yellow:     ansi.Hex("#825600"),
		Blue:       ansi.Hex("#075ec5"),
		Magenta:    ansi.Hex("#793ed0"),
		Cyan:       ansi.Hex("#006699"),
		White:      ansi.Hex("#262b33"),
		BrBlack:    ansi.Hex("#525b66"),
		BrRed:      ansi.Hex("#b11927"),
		BrGreen:    ansi.Hex("#086428"),
		BrYellow:   ansi.Hex("#795100"),
		BrBlue:     ansi.Hex("#0052b0"),
		BrMagenta:  ansi.Hex("#6b32bd"),
		BrCyan:     ansi.Hex("#005e8f"),
		BrWhite:    ansi.Hex("#202631"),
	}
	LogoColors = [...]ansi.Color{
		ansi.Hex("#262b33"),
		ansi.Hex("#3b424d"),
		ansi.Hex("#505866"),
		ansi.Hex("#667080"),
		ansi.Hex("#7c8799"),
		ansi.Hex("#939eb0"),
	}
}
