package theme

import (
	"math"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func TestSignatureIncludesFullPalette(t *testing.T) {
	previous := Default
	t.Cleanup(func() { Default = previous })
	SetDark()
	base := Default.Signature()
	changed := Default
	changed.BrWhite.R++

	if base == changed.Signature() {
		t.Fatal("signature ignored bright white")
	}
	if strings.Contains(base, "%!") {
		t.Fatalf("malformed signature: %q", base)
	}
}

// Secondary labels and status/syntax colors must remain readable even on the
// selected row, where the old light palette's muted text lost most contrast.
func TestLightTextContrastOnSurfaces(t *testing.T) {
	previous := Default
	t.Cleanup(func() { Default = previous })
	SetLight()
	luminance := func(c ansi.Color) float64 {
		channel := func(v uint8) float64 {
			x := float64(v) / 255
			if x <= 0.04045 {
				return x / 12.92
			}
			return math.Pow((x+0.055)/1.055, 2.4)
		}
		return 0.2126*channel(c.R) + 0.7152*channel(c.G) + 0.0722*channel(c.B)
	}
	for _, bg := range []ansi.Color{Default.Background, Default.Surface, Default.Selection} {
		for _, fg := range []ansi.Color{Default.Foreground, Default.BrBlack, Default.Red, Default.Green, Default.Yellow, Default.Blue, Default.Magenta, Default.Cyan} {
			contrast := (luminance(bg) + 0.05) / (luminance(fg) + 0.05)
			if contrast < 4.5 {
				t.Errorf("%06x text on %06x background has %.2f contrast; want at least 4.5", fg.Hex(), bg.Hex(), contrast)
			}
		}
	}
}
