package text_test

import (
	"strings"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/text"
)

func TestTruncateHeadEmpty(t *testing.T) {
	if got := TruncateHead("", 100); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestTruncateHeadUnderCap(t *testing.T) {
	in := "hello world"
	if got := TruncateHead(in, 100); got != in {
		t.Errorf("expected unchanged, got %q", got)
	}
}

func TestTruncateHeadExactCap(t *testing.T) {
	in := strings.Repeat("a", 100)
	if got := TruncateHead(in, 100); got != in {
		t.Errorf("expected unchanged at exact cap, got len=%d", len(got))
	}
}

func TestTruncateHeadOverCap(t *testing.T) {
	in := strings.Repeat("a", 1000)
	got := TruncateHead(in, 100)

	if !strings.HasPrefix(got, strings.Repeat("a", 100)) {
		t.Errorf("expected 100-char head, got prefix %q", got[:110])
	}
	if !strings.Contains(got, "900 chars truncated") {
		t.Errorf("expected '900 chars truncated' in result, got %q", got)
	}
}

func TestTruncateHeadUTF8Boundary(t *testing.T) {

	in := "ABC☃X"
	got := TruncateHead(in, 4)

	if !strings.HasPrefix(got, "ABC") {
		t.Errorf("expected 'ABC' prefix, got %q", got)
	}

	if strings.HasPrefix(got, "ABC☃") {
		t.Errorf("kept partial multibyte rune; got %q", got)
	}
	if !strings.Contains(got, "2 chars truncated") {
		t.Errorf("expected '2 chars truncated', got %q", got)
	}
}

func TestTruncateHeadZeroBudget(t *testing.T) {
	in := strings.Repeat("a", 100)
	got := TruncateHead(in, 0)

	if !strings.Contains(got, "100 chars truncated") {
		t.Errorf("expected full count in marker, got %q", got)
	}
}

func TestTruncateHeadRemovedCharsCount(t *testing.T) {

	in := strings.Repeat("☃", 100)
	got := TruncateHead(in, 30)

	for n := 88; n <= 92; n++ {
		if strings.Contains(got, itoa(n)+" chars truncated") {
			return
		}
	}
	t.Errorf("expected dropped-char count near 90 (not byte count), got %q", got)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestHeadLinesEndsAtLineBoundary(t *testing.T) {
	in := "first\nsecond\nthird\n"
	for n, want := range map[int]string{
		len(in):     in,
		len(in) - 1: "first\nsecond\nthird",
		12:          "first\nsecond",
		11:          "first",
		5:           "first",
		4:           "firs", // the first line alone exceeds the budget
		0:           "",
	} {
		if got := HeadLines(in, n); got != want {
			t.Errorf("HeadLines(%d) = %q, want %q", n, got, want)
		}
	}
	if got := HeadLines("日本語\n🙂", 4); got != "日" {
		t.Errorf("HeadLines split a rune: %q", got)
	}
}

func TestTailLinesStartsAtLineBoundary(t *testing.T) {
	in := "first\nsecond\nthird"
	for n, want := range map[int]string{
		len(in):     in,
		len(in) - 1: "second\nthird",
		12:          "second\nthird",
		11:          "third",
		5:           "third",
		4:           "hird", // the last line alone exceeds the budget
		0:           "",
	} {
		if got := TailLines(in, n); got != want {
			t.Errorf("TailLines(%d) = %q, want %q", n, got, want)
		}
	}
	if got := TailLines("one\ntwo\n", 4); got != "two\n" {
		t.Errorf("TailLines kept a partial line or dropped the tail: %q", got)
	}
	if got := TailLines("body\nAPI.\n\n", 5); got != "PI.\n\n" {
		t.Errorf("TailLines returned only blank lines or dropped the tail: %q", got)
	}
	if got := HeadLines("\n\nbody", 3); got != "\n\nb" {
		t.Errorf("HeadLines returned only blank lines: %q", got)
	}
	if got := TailLines("🙂\n日本語", 4); got != "語" {
		t.Errorf("TailLines split a rune: %q", got)
	}
}
