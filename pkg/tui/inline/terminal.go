package inline

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

type Pos struct {
	Row int
	Col int
}

// Terminal renders an application pinned to the bottom rows of the normal
// screen buffer. Finalized content is flushed above the live region and
// becomes regular terminal scrollback; the live region is diff-rendered in
// place with absolute positioning. All methods must be called from a single
// goroutine.
type Terminal struct {
	in     *os.File
	reader io.Reader
	out    io.Writer

	events chan Event
	done   chan struct{}

	width  int
	height int

	live []string

	alt      bool
	altFrame []string

	oldState *term.State
	sizeFn   func() (int, int)
}

type Option func(*Terminal)

// WithIO replaces the terminal's reader/writer, disabling raw-mode handling —
// used by tests.
func WithIO(r io.Reader, w io.Writer, size func() (int, int)) Option {
	return func(t *Terminal) {
		t.in = nil
		t.reader = r
		t.out = w
		t.sizeFn = size
	}
}

func NewTerminal(opts ...Option) *Terminal {
	t := &Terminal{
		in:     os.Stdin,
		reader: os.Stdin,
		out:    os.Stdout,
		events: make(chan Event, 32),
		done:   make(chan struct{}),
	}

	for _, opt := range opts {
		opt(t)
	}

	return t
}

func (t *Terminal) Events() <-chan Event {
	return t.events
}

func (t *Terminal) Size() (int, int) {
	return t.width, t.height
}

func (t *Terminal) Start() error {
	if t.in != nil {
		state, err := term.MakeRaw(int(t.in.Fd()))
		if err != nil {
			return err
		}
		t.oldState = state

		if err := setupConsole(); err != nil {
			term.Restore(int(t.in.Fd()), t.oldState)
			return err
		}
	}

	t.width, t.height = t.querySize()

	fmt.Fprint(t.out, "\x1b[?2004h")

	startInput(t.reader, t.events, t.done)
	t.watchResize()

	return nil
}

func (t *Terminal) Stop() {
	close(t.done)

	if t.alt {
		fmt.Fprint(t.out, "\x1b[?1049l")
		t.alt = false
	}

	if len(t.live) > 0 {
		fmt.Fprintf(t.out, "\x1b[%d;1H\r\n", t.height)
	}

	fmt.Fprint(t.out, "\x1b[?2004l\x1b[0m\x1b[?25h")

	if t.in != nil && t.oldState != nil {
		term.Restore(int(t.in.Fd()), t.oldState)
	}
}

func (t *Terminal) querySize() (int, int) {
	if t.sizeFn != nil {
		return t.sizeFn()
	}
	if t.in != nil {
		if w, h, err := term.GetSize(int(t.in.Fd())); err == nil && w > 0 {
			return w, h
		}
	}
	return 80, 24
}

// checkResize re-queries the terminal size and posts a ResizeEvent when it
// changed; called from the platform resize watcher.
func (t *Terminal) checkResize() {
	w, h := t.querySize()
	if w != t.width || h != t.height {
		select {
		case t.events <- ResizeEvent{Width: w, Height: h}:
		case <-t.done:
		}
	}
}

// Resized must be called by the app when it receives a ResizeEvent, before
// re-rendering.
func (t *Terminal) Resized(w, h int) {
	t.width = w
	t.height = h

	if t.alt {
		t.altFrame = nil
		return
	}

	// The terminal reflowed our rows; wipe the screen and repaint from
	// scratch (scrollback above is untouched).
	fmt.Fprint(t.out, "\x1b[2J")
	t.live = nil
}

func (t *Terminal) row(cmd string, row int) {
	fmt.Fprintf(t.out, "\x1b[%d;1H%s", row, cmd)
}

func (t *Terminal) top(count int) int {
	top := t.height - count + 1
	if top < 1 {
		top = 1
	}
	return top
}

// paint writes lines into the bottom rows, assuming those rows are available.
// prev holds the lines previously painted at the same rows, index-aligned
// (nil forces a full repaint).
func (t *Terminal) paint(lines []string, prev []string) {
	top := t.top(len(lines))

	for i, line := range lines {
		if i < len(prev) && prev[i] == line {
			continue
		}
		t.row("\x1b[2K"+line+"\x1b[0m", top+i)
	}

	t.live = append([]string(nil), lines...)
}

// Render draws the live region pinned to the bottom of the screen. Lines must
// be pre-wrapped to the terminal width. cursor, when non-nil, positions the
// hardware cursor within the live region (row is an index into lines).
func (t *Terminal) Render(lines []string, cursor *Pos) {
	if t.alt {
		return
	}

	if max := t.height; len(lines) > max && max > 0 {
		lines = lines[len(lines)-max:]
		if cursor != nil && cursor.Row >= len(lines) {
			cursor = &Pos{Row: len(lines) - 1, Col: cursor.Col}
		}
	}

	fmt.Fprint(t.out, "\x1b[?2026h\x1b[?25l")

	oldLen := len(t.live)
	newLen := len(lines)

	// alignedPrev holds what currently sits on the rows the new frame will
	// occupy, index-aligned to the new frame.
	alignedPrev := t.live

	if newLen > oldLen {
		// Scroll prior content up to make room; the old region rows shift
		// into alignment with the new anchor.
		t.row("", t.height)
		fmt.Fprint(t.out, strings.Repeat("\n", newLen-oldLen))
	}

	if newLen < oldLen {
		for row := t.top(oldLen); row < t.top(newLen); row++ {
			t.row("\x1b[2K", row)
		}
		alignedPrev = t.live[oldLen-newLen:]
	}

	t.paint(lines, alignedPrev)

	if cursor != nil {
		fmt.Fprintf(t.out, "\x1b[%d;%dH\x1b[?25h", t.top(newLen)+cursor.Row, cursor.Col+1)
	}

	fmt.Fprint(t.out, "\x1b[?2026l")
}

// Flush appends finalized lines above the live region; they scroll into the
// terminal's own scrollback and are never touched again.
func (t *Terminal) Flush(lines []string) {
	if t.alt || len(lines) == 0 {
		return
	}

	fmt.Fprint(t.out, "\x1b[?2026h\x1b[?25l")

	oldLen := len(t.live)
	top := t.top(oldLen)

	for row := top; row <= t.height; row++ {
		t.row("\x1b[2K", row)
	}

	t.row("", top)
	cursorRow := top
	for _, line := range lines {
		fmt.Fprint(t.out, "\x1b[2K", line, "\x1b[0m\r\n")
		if cursorRow < t.height {
			cursorRow++
		}
	}

	// Ensure enough blank rows below the history for the live region.
	needed := oldLen
	if avail := t.height - cursorRow + 1; needed > avail {
		t.row("", t.height)
		fmt.Fprint(t.out, strings.Repeat("\n", needed-avail))
	}

	prev := append([]string(nil), t.live...)
	t.live = nil
	t.paint(prev, nil)

	fmt.Fprint(t.out, "\x1b[?2026l")
}

func (t *Terminal) EnterAlt() {
	if t.alt {
		return
	}
	t.alt = true
	t.altFrame = nil
	fmt.Fprint(t.out, "\x1b[?1049h\x1b[?25l\x1b[2J\x1b[H")
}

func (t *Terminal) ExitAlt() {
	if !t.alt {
		return
	}
	t.alt = false
	fmt.Fprint(t.out, "\x1b[?1049l\x1b[2J")

	prev := append([]string(nil), t.live...)
	t.live = nil
	t.paint(prev, nil)
}

// RenderAlt draws a full-screen frame in the alternate buffer.
func (t *Terminal) RenderAlt(lines []string) {
	if !t.alt {
		return
	}

	fmt.Fprint(t.out, "\x1b[?2026h")

	for i := 0; i < t.height; i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		if i < len(t.altFrame) && t.altFrame[i] == line {
			continue
		}
		fmt.Fprintf(t.out, "\x1b[%d;1H\x1b[2K%s\x1b[0m", i+1, line)
	}

	t.altFrame = append(t.altFrame[:0], lines...)

	fmt.Fprint(t.out, "\x1b[?2026l")
}
