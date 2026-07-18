package ansi

import (
	"reflect"
	"testing"
)

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
