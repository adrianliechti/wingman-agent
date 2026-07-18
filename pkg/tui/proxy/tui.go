package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/proxy"
	"github.com/adrianliechti/wingman-agent/pkg/tui"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

const (
	pageStart  = "start"
	pageList   = "list"
	pageDetail = "detail"
)

type App struct {
	p    *proxy.Proxy
	term *inline.Terminal

	queue chan func()
	quit  chan struct{}
	once  sync.Once

	activePage string
	selected   int
	selectedID string

	detailLines  []string
	detailScroll int

	statusText   string
	seenRequests int
}

func newApp(p *proxy.Proxy) *App {
	return &App{
		p:          p,
		term:       inline.NewTerminal(),
		queue:      make(chan func(), 16),
		quit:       make(chan struct{}),
		activePage: pageStart,
	}
}

func (a *App) post(fn func()) {
	select {
	case a.queue <- fn:
	case <-a.quit:
	}
}

func (a *App) stop() {
	a.once.Do(func() { close(a.quit) })
}

func (a *App) Run() error {
	if err := a.term.Start(); err != nil {
		return err
	}
	a.term.EnterAlt()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	a.render()

	for {
		select {
		case <-a.quit:
			a.term.Stop()
			return nil

		case fn := <-a.queue:
			fn()

		case <-ticker.C:
			entries := a.p.Store.List()

			if a.activePage == pageStart && len(entries) > 0 && a.seenRequests == 0 {
				a.activePage = pageList
			}
			a.seenRequests = len(entries)

		case ev := <-a.term.Events():
			switch ev := ev.(type) {
			case inline.ResizeEvent:
				a.term.Resized(ev.Width, ev.Height)
			case inline.KeyEvent:
				if a.handleKey(ev) {
					a.term.Stop()
					return nil
				}
			}
		}

		a.render()
	}
}

func (a *App) handleKey(ev inline.KeyEvent) bool {
	_, height := a.term.Size()
	entries := a.p.Store.List()

	quitKey := ev.Key == inline.KeyCtrl && ev.Rune == 'c' ||
		ev.Key == inline.KeyRune && ev.Rune == 'q'

	switch a.activePage {
	case pageStart:
		switch {
		case quitKey, ev.Key == inline.KeyEsc:
			return true
		case ev.Key == inline.KeyEnter, ev.Key == inline.KeyRune && ev.Rune == 'l':
			a.activePage = pageList
		}

	case pageList:
		switch {
		case quitKey:
			return true
		case ev.Key == inline.KeyUp:
			if a.selected > 0 {
				a.selected--
			}
		case ev.Key == inline.KeyDown:
			if a.selected < len(entries)-1 {
				a.selected++
			}
		case ev.Key == inline.KeyEnter:
			if idx := len(entries) - 1 - a.selected; idx >= 0 && idx < len(entries) {
				a.selectedID = entries[idx].ID
				a.renderDetail()
				a.activePage = pageDetail
			}
		case ev.Key == inline.KeyRune && ev.Rune == 's':
			if idx := len(entries) - 1 - a.selected; idx >= 0 && idx < len(entries) {
				a.saveEntry(entries[idx])
			}
		}

	case pageDetail:
		switch {
		case quitKey:
			return true
		case ev.Key == inline.KeyEsc:
			a.activePage = pageList
		case ev.Key == inline.KeyUp:
			a.detailScroll--
		case ev.Key == inline.KeyDown:
			a.detailScroll++
		case ev.Key == inline.KeyPgUp:
			a.detailScroll -= height - 2
		case ev.Key == inline.KeyPgDn, ev.Key == inline.KeyRune && ev.Rune == ' ':
			a.detailScroll += height - 2
		case ev.Key == inline.KeyRune && ev.Rune == 's':
			if entry, ok := a.p.Store.Get(a.selectedID); ok {
				a.saveEntry(entry)
			}
		}

		if maxScroll := len(a.detailLines) - (height - 2); a.detailScroll > maxScroll {
			a.detailScroll = maxScroll
		}
		if a.detailScroll < 0 {
			a.detailScroll = 0
		}
	}

	return false
}

func (a *App) render() {
	width, height := a.term.Size()
	if width <= 0 || height <= 0 {
		return
	}

	var lines []string

	switch a.activePage {
	case pageStart:
		lines = a.startLines(width)
	case pageList:
		lines = a.listLines(width, height-1)
		lines = append(lines, a.statusLine(width))
	case pageDetail:
		end := a.detailScroll + height - 1
		if end > len(a.detailLines) {
			end = len(a.detailLines)
		}
		start := a.detailScroll
		if start > end {
			start = end
		}
		lines = append(lines, a.detailLines[start:end]...)
		for len(lines) < height-1 {
			lines = append(lines, "")
		}
		lines = append(lines, a.statusLine(width))
	}

	a.term.RenderAlt(lines, nil)
}

func (a *App) startLines(width int) []string {
	th := theme.Default

	var lines []string
	lines = append(lines, "")
	lines = append(lines, "  "+ansi.Bold+"wingman proxy"+ansi.Reset)
	lines = append(lines, "")
	lines = append(lines, "  "+ansi.Fg(th.Yellow)+ansi.Bold+"Listening"+ansi.Reset+"  http://"+a.p.Addr)
	lines = append(lines, "")
	lines = append(lines, "  "+ansi.Fg(th.Cyan)+ansi.Bold+"Usage"+ansi.Reset)
	lines = append(lines, "  "+dim("Point your OpenAI client to the proxy:"))
	lines = append(lines, "")
	lines = append(lines, "  "+ansi.Fg(th.Green)+"export OPENAI_BASE_URL=http://"+a.p.Addr+"/v1"+ansi.Reset)
	lines = append(lines, "  "+ansi.Fg(th.Green)+"export OPENAI_API_KEY=any-value"+ansi.Reset)
	lines = append(lines, "")
	lines = append(lines, "  "+dim("enter/l requests · q quit"))

	return lines
}

func dim(text string) string {
	return ansi.Fg(theme.Default.BrBlack) + text + ansi.Reset
}

func (a *App) listLines(width, height int) []string {
	th := theme.Default
	entries := a.p.Store.List()

	pathWidth := width - 8 - 7 - 5 - 7 - 22 - 7 - 7 - 9*2
	if pathWidth < 12 {
		pathWidth = 12
	}

	format := func(time_, method, path, status, dur, model, in, out string) string {
		return "  " + ansi.Pad(time_, 8) + " " + ansi.Pad(method, 6) + " " +
			ansi.Pad(path, pathWidth) + " " + ansi.Pad(status, 4) + " " +
			ansi.Pad(dur, 6) + " " + ansi.Pad(model, 20) + " " +
			ansi.Pad(in, 6) + " " + ansi.Pad(out, 6)
	}

	lines := []string{
		"",
		dim(format("Time", "Method", "Path", "St", "Dur", "Model", "In", "Out")),
	}

	if a.selected >= len(entries) {
		a.selected = max(0, len(entries)-1)
	}

	rows := height - 2

	start := 0
	if a.selected >= rows {
		start = a.selected - rows + 1
	}

	for i := start; i < len(entries) && i < start+rows; i++ {
		e := entries[len(entries)-1-i]

		statusColor := th.Green
		statusText := fmt.Sprintf("%d", e.Status)
		switch {
		case e.Status == 0:
			statusColor, statusText = th.Red, "ERR"
		case e.Status >= 500:
			statusColor = th.Red
		case e.Status >= 400:
			statusColor = th.Yellow
		}

		dur := fmt.Sprintf("%dms", e.Duration.Milliseconds())
		if e.Duration >= time.Second {
			dur = fmt.Sprintf("%.1fs", e.Duration.Seconds())
		}

		row := "  " + dim(ansi.Pad(e.Timestamp.Format("15:04:05"), 8)) + " " +
			ansi.Fg(th.Magenta) + ansi.Pad(e.Method, 6) + ansi.Reset + " " +
			ansi.Pad(requestURLPathText(e.URL), pathWidth) + " " +
			ansi.Fg(statusColor) + ansi.Pad(statusText, 4) + ansi.Reset + " " +
			dim(ansi.Pad(dur, 6)) + " " +
			ansi.Fg(th.Cyan) + ansi.Pad(e.Model, 20) + ansi.Reset + " " +
			dim(ansi.Pad(tui.FormatTokens(int64(e.InputTokens)), 6)) + " " +
			dim(ansi.Pad(tui.FormatTokens(int64(e.OutputTokens)), 6))

		if i == a.selected {
			row = ansi.Bg(th.Selection) + row + ansi.Reset
		}

		lines = append(lines, row)
	}

	return lines
}

func (a *App) statusLine(width int) string {
	th := theme.Default

	if a.statusText != "" {
		return "  " + ansi.Fg(th.Red) + a.statusText + ansi.Reset
	}

	entries := a.p.Store.List()
	inputTotal, outputTotal := a.p.Store.TotalTokens()

	parts := []string{
		ansi.Fg(th.Blue) + ansi.Bold + "⇆ " + a.p.Addr + ansi.Reset,
		fmt.Sprintf("%d requests", len(entries)),
	}

	if inputTotal > 0 || outputTotal > 0 {
		parts = append(parts, ansi.Fg(th.Cyan)+tui.FormatTokens(int64(inputTotal))+" in / "+tui.FormatTokens(int64(outputTotal))+" out"+ansi.Reset)
	}

	parts = append(parts, dim("enter detail · s save · q quit"))

	return "  " + strings.Join(parts, dim(" · "))
}

func (a *App) renderDetail() {
	th := theme.Default
	a.detailScroll = 0

	entry, ok := a.p.Store.Get(a.selectedID)
	if !ok {
		a.detailLines = []string{"", "  " + ansi.Fg(th.Red) + "Request not found" + ansi.Reset}
		return
	}

	statusColor := th.Green
	if entry.Status >= 400 {
		statusColor = th.Yellow
	}
	if entry.Status >= 500 || entry.Status == 0 {
		statusColor = th.Red
	}

	var lines []string

	field := func(label string, value string) {
		lines = append(lines, "  "+dim(ansi.Pad(label, 9))+" "+value)
	}

	lines = append(lines, "")
	lines = append(lines, "  "+ansi.Bold+"Request Detail"+ansi.Reset)
	lines = append(lines, "")

	field("Method", ansi.Fg(th.Magenta)+entry.Method+ansi.Reset)
	field("URL", requestURLText(entry.URL))
	field("Status", ansi.Fg(statusColor)+fmt.Sprintf("%d", entry.Status)+ansi.Reset)
	field("Duration", entry.Duration.Round(time.Millisecond).String())
	field("Model", ansi.Fg(th.Cyan)+entry.Model+ansi.Reset)

	if entry.InputTokens > 0 || entry.OutputTokens > 0 {
		tokens := tui.FormatTokens(int64(entry.InputTokens)) + " in"
		if entry.CachedTokens > 0 {
			tokens += " (" + tui.FormatTokens(int64(entry.CachedTokens)) + " cached)"
		}
		tokens += " / " + tui.FormatTokens(int64(entry.OutputTokens)) + " out"
		field("Tokens", ansi.Fg(th.Cyan)+tokens+ansi.Reset)
	}

	if entry.Error != "" {
		field("Error", ansi.Fg(th.Red)+entry.Error+ansi.Reset)
	}

	if len(entry.RequestBody) > 0 {
		lines = append(lines, "")
		lines = append(lines, "  "+ansi.Fg(th.Yellow)+ansi.Bold+"─── Request Body ───"+ansi.Reset)
		lines = append(lines, "")
		lines = append(lines, formatJSON(entry.RequestBody)...)
	}

	if len(entry.ResponseBody) > 0 {
		lines = append(lines, "")
		lines = append(lines, "  "+ansi.Fg(th.Yellow)+ansi.Bold+"─── Response Body ───"+ansi.Reset)
		lines = append(lines, "")

		if !looksLikeJSON(entry.ResponseBody) {
			lines = append(lines, formatSSEBody(entry.ResponseBody)...)
		} else {
			lines = append(lines, formatJSON(entry.ResponseBody)...)
		}
	}

	a.detailLines = lines
}

func requestURLText(u *url.URL) string {
	if u == nil {
		return ""
	}

	return u.String()
}

func requestURLPathText(u *url.URL) string {
	if u == nil {
		return ""
	}

	return u.Path
}

func bodyLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		lines = append(lines, "  "+strings.ReplaceAll(line, "\t", "  "))
	}
	return lines
}

func formatJSON(data []byte) []string {
	var pretty bytes.Buffer

	if json.Indent(&pretty, data, "", "  ") == nil {
		return bodyLines(pretty.String())
	}

	return bodyLines(string(data))
}

func formatSSEBody(data []byte) []string {
	var lines []string

	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if after, ok := strings.CutPrefix(line, "data: "); ok {
			payload := after
			if payload == "[DONE]" {
				lines = append(lines, "  "+dim("data: [DONE]"))
				continue
			}

			var pretty bytes.Buffer
			if json.Indent(&pretty, []byte(payload), "", "  ") == nil {
				lines = append(lines, bodyLines("data: "+pretty.String())...)
			} else {
				lines = append(lines, bodyLines(line)...)
			}
		} else {
			lines = append(lines, "  "+dim(line))
		}
	}

	return lines
}

func (a *App) saveEntry(entry proxy.RequestEntry) {
	name := fmt.Sprintf("%s.jsonl", entry.Timestamp.Format("20060102_150405"))

	if err := os.WriteFile(name, buildSavedEntry(entry), 0644); err != nil {
		a.statusText = fmt.Sprintf("Save failed: %v", err)
	}
}

func buildSavedEntry(entry proxy.RequestEntry) []byte {
	var buf strings.Builder

	for i, body := range [][]byte{entry.RequestBody, entry.ResponseBody} {
		if len(body) == 0 {
			continue
		}

		if i > 0 && buf.Len() > 0 {
			fmt.Fprint(&buf, "\n")
		}

		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "", "  ") == nil {
			fmt.Fprint(&buf, pretty.String())
		} else {
			buf.Write(body)
		}

		fmt.Fprint(&buf, "\n")
	}

	return []byte(buf.String())
}

func looksLikeJSON(data []byte) bool {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}

	return false
}
