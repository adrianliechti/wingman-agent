package inline

import (
	"strings"
	"testing"
)

// vtScreen is a minimal terminal emulator covering the sequences the
// renderer emits: CUP, EL, ED, LF-scrolling.
type vtScreen struct {
	rows []string
	row  int
	h    int
}

func newVTScreen(h int) *vtScreen {
	return &vtScreen{rows: make([]string, h), row: 0, h: h}
}

func (s *vtScreen) Write(p []byte) (int, error) {
	text := string(p)

	for len(text) > 0 {
		if text[0] == 0x1b {
			seq, rest := readEsc(text)
			s.apply(seq)
			text = rest
			continue
		}

		switch text[0] {
		case '\n':
			if s.row >= s.h-1 {
				s.rows = append(s.rows[1:], "")
				s.row = s.h - 1
			} else {
				s.row++
			}
			text = text[1:]
		case '\r':
			text = text[1:]
		default:
			idx := strings.IndexAny(text, "\x1b\n\r")
			if idx < 0 {
				idx = len(text)
			}
			s.rows[s.row] += text[:idx]
			text = text[idx:]
		}
	}

	return len(p), nil
}

func (s *vtScreen) apply(seq string) {
	if !strings.HasPrefix(seq, "\x1b[") {
		return
	}
	body := seq[2 : len(seq)-1]
	final := seq[len(seq)-1]

	switch final {
	case 'H':
		row := 1
		if body != "" {
			parts := strings.SplitN(body, ";", 2)
			for i, r := 0, 0; i < len(parts[0]); i++ {
				r = r*10 + int(parts[0][i]-'0')
				row = r
			}
		}
		if row > s.h {
			row = s.h
		}
		s.row = row - 1
	case 'K':
		s.rows[s.row] = ""
	case 'J':
		if body == "2" {
			for i := range s.rows {
				s.rows[i] = ""
			}
		}
	}
}

func readEsc(text string) (string, string) {
	for i := 2; i < len(text); i++ {
		b := text[i]
		if b >= 0x40 && b <= 0x7e {
			return text[:i+1], text[i+1:]
		}
	}
	return text, ""
}

func newTestTerminal(h int) (*Terminal, *vtScreen) {
	screen := newVTScreen(h)
	t := NewTerminal(WithIO(strings.NewReader(""), screen, func() (int, int) { return 40, h }))
	t.width, t.height = 40, h
	return t, screen
}

func TestRenderPinsToBottom(t *testing.T) {
	term, screen := newTestTerminal(10)

	term.Render([]string{"one", "two", "three"}, nil)

	if screen.rows[7] != "one" || screen.rows[8] != "two" || screen.rows[9] != "three" {
		t.Fatalf("rows = %q", screen.rows)
	}
	for i := 0; i < 7; i++ {
		if screen.rows[i] != "" {
			t.Fatalf("row %d not empty: %q", i, screen.rows[i])
		}
	}
}

func TestRenderGrowAndShrinkStaysPinned(t *testing.T) {
	term, screen := newTestTerminal(10)

	term.Render([]string{"a", "b"}, nil)
	term.Render([]string{"a", "b", "c", "d"}, nil)

	if screen.rows[6] != "a" || screen.rows[9] != "d" {
		t.Fatalf("after grow: %q", screen.rows)
	}

	term.Render([]string{"x", "y"}, nil)

	if screen.rows[8] != "x" || screen.rows[9] != "y" {
		t.Fatalf("after shrink: %q", screen.rows)
	}
	if screen.rows[6] != "" || screen.rows[7] != "" {
		t.Fatalf("stale rows after shrink: %q", screen.rows)
	}
}

func TestFlushInsertsAboveLiveRegion(t *testing.T) {
	term, screen := newTestTerminal(6)

	term.Render([]string{"live1", "live2"}, nil)
	term.Flush([]string{"hist1", "hist2", "hist3"})

	if screen.rows[4] != "live1" || screen.rows[5] != "live2" {
		t.Fatalf("live region not at bottom: %q", screen.rows)
	}

	joined := strings.Join(screen.rows, "\n")
	h1 := strings.Index(joined, "hist1")
	h3 := strings.Index(joined, "hist3")
	l1 := strings.Index(joined, "live1")

	if h1 < 0 || h3 < 0 || h1 > h3 || h3 > l1 {
		t.Fatalf("history misordered: %q", screen.rows)
	}
}

func TestFlushScrollsHistoryIntoScrollback(t *testing.T) {
	term, screen := newTestTerminal(5)

	term.Render([]string{"live"}, nil)

	var hist []string
	for _, s := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
		hist = append(hist, s)
	}
	term.Flush(hist)

	if screen.rows[4] != "live" {
		t.Fatalf("live region lost: %q", screen.rows)
	}

	joined := strings.Join(screen.rows, "\n")
	if !strings.Contains(joined, "h6") {
		t.Fatalf("latest history not visible: %q", screen.rows)
	}
}
