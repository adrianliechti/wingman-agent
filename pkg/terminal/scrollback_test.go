package terminal

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

func TestSafeTailStartsAtABoundary(t *testing.T) {
	tests := []struct {
		name, data, want string
		limit            int
	}{
		{name: "short", data: "abc", limit: 10, want: "abc"},
		{name: "after newline", data: "first line\r\nsecond\r\nthird", limit: 15, want: "second\r\nthird"},
		{name: "at escape", data: "xxxxxxxx\x1b[31mred", limit: 9, want: "\x1b[31mred"},
		{name: "utf-8", data: "aa🌍🌍🌍", limit: 9, want: "🌍🌍"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SafeTail([]byte(test.data), test.limit)
			if string(got) != test.want || len(got) > test.limit {
				t.Fatalf("SafeTail(%q, %d) = %q, want %q", test.data, test.limit, got, test.want)
			}
		})
	}
	long := bytes.Repeat([]byte("é"), 10000)
	if got := SafeTail(long, 777); !utf8.Valid(got) || len(got) > 777 {
		t.Fatalf("tail without newline or escape split a character: %d bytes", len(got))
	}
}
