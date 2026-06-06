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
