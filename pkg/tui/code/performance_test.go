package code

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/changes"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func BenchmarkDiffPanelLargePatch(b *testing.B) {
	for _, scenario := range []struct {
		name string
		body string
	}{
		{"minified", strings.Repeat("const value = 123;", 200_000)},
		{"many_lines", strings.Repeat("+const value = 123;\n", 20_000)},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			diffs := []changes.FileDiff{{Path: "bundle.js", Status: changes.StatusModified, Patch: "@@ -0,0 +1 @@\n+" + scenario.body}}
			b.ReportAllocs()
			for b.Loop() {
				panel := diffPanel{diffs: diffs}
				panel.header, panel.lines, panel.sections = renderDiffPanelLines(diffs, nil, 79)
				panel.render(80, 24)
			}
		})
	}
}

func BenchmarkTUIRender(b *testing.B) {
	for _, scenario := range []struct {
		name   string
		stream string
	}{
		{"idle", ""},
		{"long_stream", strings.Repeat("A **streaming reply** with `inline code` and a [link](https://example.com).\n\n", 100)},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			term := inline.NewTerminal(inline.WithIO(strings.NewReader(""), io.Discard, func() (int, int) { return 120, 40 }))
			term.Resized(120, 40)
			term.EnterAlt()
			a := &App{ctx: context.Background(), agent: newUITestAgent(nil), sessionID: "session", editor: NewEditor(), follow: true}
			a.WithTerminal(term)
			a.chat = make([]string, 10_000)
			for i := range a.chat {
				a.chat[i] = "A committed line in a long conversation."
			}
			a.streamCurrent.text = scenario.stream
			a.render()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				a.render()
			}
		})
	}
}

func BenchmarkEditorRender(b *testing.B) {
	editor := NewEditor()
	editor.SetText(strings.Repeat("A long pasted prompt with source context.\n", 500))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		editor.Render(120, 10, EditorChrome{})
	}
}
