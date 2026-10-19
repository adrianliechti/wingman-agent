package ansi

import (
	"reflect"
	"strings"
	"testing"
)

func TestCutPlain(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		from int
		to   int
		want string
	}{
		{"range", "abcdef", 1, 4, "bcd"},
		{"empty", "abc", 2, 2, ""},
		{"past end", "abc", 5, 10, ""},
		{"styling", "\x1b[31mred\x1b[0m text", 1, 5, "ed t"},
		{"hyperlink", "\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\!", 1, 5, "ink!"},
		{"combining marks", "ae\u0301bc", 1, 3, "e\u0301b"},
		{"wide cluster at end", "a界bc", 0, 2, "a界"},
		{"inside wide cluster", "a界bc", 2, 4, "b"},
		{"emoji", "a👩‍💻bc", 1, 3, "👩‍💻"},
		{"large suffix", "prefix" + strings.Repeat("x", 3_600_000), 0, 6, "prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CutPlain(tc.text, tc.from, tc.to); got != tc.want {
				t.Fatalf("CutPlain = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWrap(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  []string
	}{
		{"hello world", 5, []string{"hello", "world"}},
		{"hello  world", 5, []string{"hello", "world"}},
		{"x := f()  // note", 8, []string{"x := f()", "// note"}},
		{"ab  cd", 2, []string{"ab", "cd"}},
		{"  indented code", 20, []string{"  indented code"}},
		{"a b c", 3, []string{"a b", "c"}},
		{"abcdef", 3, []string{"abc", "def"}},
		{"", 10, []string{""}},
		{"\x1b[31mred text here", 4, []string{"\x1b[31mred", "\x1b[31mtext", "\x1b[31mhere"}},
	}

	for _, c := range cases {
		if got := Wrap(c.in, c.width); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Wrap(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}
