package text_test

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	. "github.com/adrianliechti/wingman-agent/pkg/text"
)

// Ported from pi's durable harness-output tests: every byte budget must cut on
// a character boundary, keep as much as fits, and never split a rune. The
// corpus is the exhaustive depth-3 alphabet of 1-4 byte runes and newlines
// plus seeded random strings, so both helpers are checked at every budget.
func boundaryCorpus() []string {
	alphabet := []string{"a", "é", "中", "🙂", "\n"}
	var corpus []string
	var build func(prefix string, depth int)
	build = func(prefix string, depth int) {
		corpus = append(corpus, prefix)
		if depth == 0 {
			return
		}
		for _, r := range alphabet {
			build(prefix+r, depth-1)
		}
	}
	build("", 3)
	rng := rand.New(rand.NewPCG(0x12345678, 0))
	for range 1000 {
		var b strings.Builder
		for range rng.IntN(12) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		corpus = append(corpus, b.String())
	}
	return corpus
}

func TestHeadTailBytesCutOnCharacterBoundaries(t *testing.T) {
	for _, s := range boundaryCorpus() {
		for n := 0; n <= len(s)+1; n++ {
			head := HeadBytes(s, n)
			if !strings.HasPrefix(s, head) || len(head) > n || !utf8.ValidString(head) {
				t.Fatalf("HeadBytes(%q, %d) = %q", s, n, head)
			}
			if rest := s[len(head):]; rest != "" {
				if _, size := utf8.DecodeRuneInString(rest); len(head)+size <= n {
					t.Fatalf("HeadBytes(%q, %d) = %q dropped a rune that fits", s, n, head)
				}
			}
			tail := TailBytes(s, n)
			if !strings.HasSuffix(s, tail) || len(tail) > n || !utf8.ValidString(tail) {
				t.Fatalf("TailBytes(%q, %d) = %q", s, n, tail)
			}
			if rest := s[:len(s)-len(tail)]; rest != "" {
				if _, size := utf8.DecodeLastRuneInString(rest); len(tail)+size <= n {
					t.Fatalf("TailBytes(%q, %d) = %q dropped a rune that fits", s, n, tail)
				}
			}
		}
	}
}

func TestHeadTailLinesCutOnLineOrCharacterBoundaries(t *testing.T) {
	for _, s := range boundaryCorpus() {
		for n := 0; n <= len(s)+1; n++ {
			head := HeadLines(s, n)
			if !strings.HasPrefix(s, head) || len(head) > n || !utf8.ValidString(head) {
				t.Fatalf("HeadLines(%q, %d) = %q", s, n, head)
			}
			if first, size := utf8.DecodeRuneInString(s); head == "" && first != utf8.RuneError && size <= n {
				t.Fatalf("HeadLines(%q, %d) kept nothing", s, n)
			}
			// Either the cut lands on a line boundary or the whole budget is
			// spent on a single line that is longer than the budget.
			if len(head) < len(s) && s[len(head)] != '\n' && head != HeadBytes(s, n) {
				t.Fatalf("HeadLines(%q, %d) = %q cut inside a line", s, n, head)
			}
			tail := TailLines(s, n)
			if !strings.HasSuffix(s, tail) || len(tail) > n || !utf8.ValidString(tail) {
				t.Fatalf("TailLines(%q, %d) = %q", s, n, tail)
			}
			if last, size := utf8.DecodeLastRuneInString(s); tail == "" && last != utf8.RuneError && size <= n {
				t.Fatalf("TailLines(%q, %d) kept nothing", s, n)
			}
			if start := len(s) - len(tail); start > 0 && s[start-1] != '\n' && tail != TailBytes(s, n) {
				t.Fatalf("TailLines(%q, %d) = %q cut inside a line", s, n, tail)
			}
		}
	}
}
