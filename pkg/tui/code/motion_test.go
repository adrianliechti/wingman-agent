package code

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func TestSparkleContinuesWhileIdleAndReturnsBetweenTurns(t *testing.T) {
	start := time.Unix(100, 0)
	a := &App{editor: NewEditor(), motion: tuiMotion{enabled: true}, termFocused: true}
	a.setPhase(PhaseIdle)
	for _, elapsed := range []time.Duration{0, 16 * time.Second, time.Minute, time.Hour} {
		lines, cursor := a.editor.Render(100, 5, EditorChrome{})
		a.motion.nextFrame = 0
		a.renderComposerSparkle(lines, cursor, 100, start.Add(elapsed))
		if a.motion.nextFrame == 0 {
			t.Fatalf("idle sparkle stopped after %s", elapsed)
		}
	}
	for _, pause := range []struct {
		name         string
		enter, leave func()
	}{
		{"typing", func() { a.editor.SetText("a draft") }, func() { a.editor.SetText("") }},
		{"turn", func() { a.setPhase(PhaseThinking) }, func() { a.setPhase(PhaseIdle) }},
		{"menu", func() { a.popup = newPopup(popupCommands, "", nil, nil) }, func() { a.popup = nil }},
		{"attachment", func() { a.pendingFiles = []string{"main.go"} }, func() { a.pendingFiles = nil }},
		{"selection", func() { a.selActive = true }, func() { a.selActive = false }},
	} {
		pause.enter()
		baseline, cursor := a.editor.Render(100, 5, EditorChrome{})
		lines := slices.Clone(baseline)
		a.motion.nextFrame = 0
		a.renderComposerSparkle(lines, cursor, 100, start.Add(2*time.Hour))
		if !slices.Equal(lines, baseline) || a.motion.nextFrame != 0 {
			t.Fatalf("sparkle continued during %s", pause.name)
		}
		pause.leave()
		lines, cursor = a.editor.Render(100, 5, EditorChrome{})
		a.renderComposerSparkle(lines, cursor, 100, start.Add(3*time.Hour))
		if a.motion.nextFrame == 0 {
			t.Fatalf("sparkle did not return after %s", pause.name)
		}
	}
}

func TestSparklePreservesComposerAndStopsSchedulingWhenHidden(t *testing.T) {
	a := &App{editor: NewEditor(), motion: tuiMotion{enabled: true}, termFocused: true}
	a.setPhase(PhaseIdle)
	start := time.Unix(100, 0)
	baseline, cursor := a.editor.Render(100, 5, EditorChrome{})
	visible := false
	for tick := range 20 {
		lines := slices.Clone(baseline)
		a.motion.nextFrame = 0
		a.renderComposerSparkle(lines, cursor, 100, start.Add(time.Duration(tick)*150*time.Millisecond))
		if lines[0] != baseline[0] || lines[len(lines)-1] != baseline[len(baseline)-1] || !strings.HasPrefix(lines[1], baseline[1]) || ansi.Width(lines[1]) > 100 {
			t.Fatalf("sparkle altered composer content or geometry: %q", lines)
		}
		visible = visible || strings.ContainsAny(lines[1], "⠁⠂⠄⠈⠐⠠⡀⢀")
		if a.motion.nextFrame == 0 {
			t.Fatal("visible flourish did not schedule its next frame")
		}
	}
	if !visible || a.editor.Text() != "" {
		t.Fatal("no sparkle was drawn or editor text was modified")
	}
	a.termFocused = false
	a.motion.nextFrame = 0
	lines := slices.Clone(baseline)
	a.renderComposerSparkle(lines, cursor, 100, start.Add(20*time.Second))
	if !slices.Equal(lines, baseline) || a.motion.nextFrame != 0 {
		t.Fatal("unfocused composer animated")
	}
	a.termFocused = true
	a.renderComposerSparkle(lines, cursor, 100, start.Add(21*time.Second))
	if a.motion.nextFrame == 0 {
		t.Fatal("focus did not resume idle sparkle")
	}
}

func TestMotionOptOutAndShimmerPreserveText(t *testing.T) {
	t.Setenv("WINGMAN_ANIMATIONS", "0")
	if newTUIMotion().enabled {
		t.Fatal("animation opt-out was ignored")
	}
	const label = "Checking 日本語 é 🦋"
	first := shimmerStatus(label, 200*time.Millisecond, theme.Default.Cyan)
	second := shimmerStatus(label, time.Second, theme.Default.Cyan)
	if first == second || ansi.Strip(first) != label || ansi.Strip(second) != label || ansi.Width(first) != ansi.Width(label) {
		t.Fatal("shimmer did not animate or changed grapheme content/width")
	}
}
