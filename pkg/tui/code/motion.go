package code

import (
	"math"
	"os"
	"strings"
	"time"

	"github.com/rivo/uniseg"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/markdown"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type tuiMotion struct {
	enabled   bool
	started   time.Time
	nextFrame time.Duration
}

func newTUIMotion() tuiMotion {
	setting := strings.ToLower(os.Getenv("WINGMAN_ANIMATIONS"))
	return tuiMotion{enabled: setting != "0" && setting != "false" && setting != "off" && os.Getenv("TERM") != "dumb" && os.Getenv("NO_COLOR") == ""}
}

func (m *tuiMotion) schedule(delay time.Duration) {
	if m.nextFrame == 0 || delay < m.nextFrame {
		m.nextFrame = delay
	}
}

// Draw only after the complete placeholder, keeping its spaces and the cursor
// untouched. Sparkle stays active while the empty composer is idle, and resumes
// when it becomes idle again. Hidden frames do not schedule animation work.
func (a *App) renderComposerSparkle(lines []string, cursor inline.Pos, width int, now time.Time) {
	if !a.motion.enabled || !a.termFocused || a.getPhase() != PhaseIdle || len(lines) < 3 {
		return
	}
	if a.popup != nil || a.overlay != nil || a.askActive || a.promptActive || a.selecting || a.selActive {
		return
	}
	if len(a.editor.value) > 0 || len(a.pendingContent) > 0 || len(a.pendingFiles) > 0 {
		return
	}
	end := max(width-len(cellIndent), 0)
	start := ansi.Width(lines[1])
	if start >= end {
		return
	}
	if a.motion.started.IsZero() {
		a.motion.started = now
	}
	elapsed := now.Sub(a.motion.started)
	var tail strings.Builder
	for col := start; col < end; col++ {
		hash := uint64(col+1) * 0x45d9f3b
		hash = (hash ^ (hash >> 16)) * 0x45d9f3b
		phase := math.Mod(elapsed.Seconds()/(4+float64(hash%31)/10)+float64(hash%997)/997, 1)
		alpha := math.Pow(math.Sin(phase*math.Pi), 12) * 0.55
		if hash%4 == 0 && alpha > 0.04 && (cursor.Row != 1 || cursor.Col != col) {
			dots := []rune("⠁⠂⠄⠈⠐⠠⡀⢀")
			tail.WriteString(colored(ansi.Blend(theme.Default.Foreground, theme.Default.Background, alpha), string(dots[(hash/17)%8])))
		} else {
			tail.WriteByte(' ')
		}
	}
	lines[1] += tail.String()
	a.motion.schedule(150 * time.Millisecond)
}

// Shimmer changes whole graphemes, so combining marks and wide characters
// remain intact. It has no bearing on the status text or elapsed work time.
func shimmerStatus(text string, elapsed time.Duration, foreground ansi.Color) string {
	t := theme.Default
	width := float64(ansi.Width(text))
	half := math.Max(3, width*0.1)
	position := math.Mod(elapsed.Seconds(), 2)/2*(width+2*half) - half
	var out strings.Builder
	column := 0.0
	base := ansi.Blend(foreground, t.Background, 0.5)
	if t.IsLight {
		// Keep the entire status readable on white; the wave shifts its hue
		// from secondary text to the activity accent instead of fading it out.
		base = t.BrBlack
	}
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		w := float64(graphemes.Width())
		distance := math.Min(math.Abs(column+w/2-position)/half, 1)
		alpha := 0.5 * (1 + math.Cos(math.Pi*distance))
		out.WriteString(colored(ansi.Blend(foreground, base, alpha), graphemes.Str()))
		column += w
	}
	return out.String()
}

func (a *App) reasoningStatus() string {
	a.streamStateMu.Lock()
	defer a.streamStateMu.Unlock()
	return lastNonEmptyLine(markdown.Sanitize(a.streamCurrent.reasoningHeadings))
}
