package ansi

import (
	"os"
	"strings"
)

// Color is a 24-bit RGB color.
type Color struct {
	R, G, B uint8
}

// Hex parses "#rrggbb"; invalid input yields black.
func Hex(s string) Color {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return Color{}
	}

	parse := func(hi, lo byte) uint8 {
		digit := func(b byte) uint8 {
			switch {
			case b >= '0' && b <= '9':
				return b - '0'
			case b >= 'a' && b <= 'f':
				return b - 'a' + 10
			case b >= 'A' && b <= 'F':
				return b - 'A' + 10
			}
			return 0
		}
		return digit(hi)<<4 | digit(lo)
	}

	return Color{R: parse(s[0], s[1]), G: parse(s[2], s[3]), B: parse(s[4], s[5])}
}

func (c Color) Hex() int32 {
	return int32(c.R)<<16 | int32(c.G)<<8 | int32(c.B)
}

const (
	Reset     = "\x1b[0m"
	Bold      = "\x1b[1m"
	Dim       = "\x1b[2m"
	Italic    = "\x1b[3m"
	Underline = "\x1b[4m"
	Reverse   = "\x1b[7m"
	Strike    = "\x1b[9m"
)

type Profile int

const (
	Profile16 Profile = iota
	Profile256
	ProfileTrue
)

var colorProfile = DetectProfile()

func SetProfile(p Profile) {
	colorProfile = p
}

func DetectProfile() Profile {
	colorterm := os.Getenv("COLORTERM")
	if strings.Contains(colorterm, "truecolor") || strings.Contains(colorterm, "24bit") {
		return ProfileTrue
	}

	term := os.Getenv("TERM")
	if term == "" {
		return ProfileTrue
	}

	return Profile256
}

func Fg(c Color) string {
	return sgrColor(c, false)
}

func Bg(c Color) string {
	return sgrColor(c, true)
}

func sgrColor(c Color, background bool) string {
	r, g, b := c.R, c.G, c.B

	var sb strings.Builder
	sb.WriteString("\x1b[")

	if background {
		sb.WriteString("48;")
	} else {
		sb.WriteString("38;")
	}

	if colorProfile == ProfileTrue {
		sb.WriteString("2;")
		writeInt(&sb, int(r))
		sb.WriteByte(';')
		writeInt(&sb, int(g))
		sb.WriteByte(';')
		writeInt(&sb, int(b))
	} else {
		sb.WriteString("5;")
		writeInt(&sb, to256(int(r), int(g), int(b)))
	}

	sb.WriteByte('m')
	return sb.String()
}

func writeInt(sb *strings.Builder, n int) {
	if n >= 100 {
		sb.WriteByte(byte('0' + n/100))
	}
	if n >= 10 {
		sb.WriteByte(byte('0' + (n/10)%10))
	}
	sb.WriteByte(byte('0' + n%10))
}

// to256 maps RGB to the xterm palette, preferring the 24-step gray ramp for
// near-neutral colors and the 6x6x6 cube otherwise.
func to256(r, g, b int) int {
	maxC := max(r, max(g, b))
	minC := min(r, min(g, b))

	if maxC-minC < 24 {
		avg := (r + g + b) / 3
		if avg < 4 {
			return 16
		}
		if avg > 246 {
			return 231
		}
		return 232 + min(23, (avg-8)/10)
	}

	step := func(v int) int {
		if v < 48 {
			return 0
		}
		if v < 115 {
			return 1
		}
		return min(5, (v-35)/40)
	}

	return 16 + 36*step(r) + 6*step(g) + step(b)
}

// Blend mixes fg into bg by alpha (0..1), used for subtle tint backgrounds
// that adapt to the theme.
func Blend(fg, bg Color, alpha float64) Color {
	mix := func(f, b uint8) uint8 {
		return uint8(float64(b) + (float64(f)-float64(b))*alpha)
	}

	return Color{R: mix(fg.R, bg.R), G: mix(fg.G, bg.G), B: mix(fg.B, bg.B)}
}
