package shell

import "testing"

func TestDecodeInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "hello"},
		{"real-bytes-untouched", "\x1b:w f\n", "\x1b:w f\n"},
		{"transcript-uHHHH-and-n", "\\u001b:w bubu.txt\\n", "\x1b:w bubu.txt\n"},
		{"e-alias", "\\e:w f\\n", "\x1b:w f\n"},
		{"ctrl-c", "\\u0003", "\x03"},
		{"tab-cr", "a\\tb\\r", "a\tb\r"},
		{"hex", "\\x1b", "\x1b"},
		{"literal-backslash", "a\\\\b", "a\\b"},
		{"unknown-escape-kept", "\\d+", "\\d+"},
		{"windows-ish-U-kept", "C:\\Users", "C:\\Users"},
		{"trailing-backslash", "foo\\", "foo\\"},
		{"bad-hex-kept", "\\xzz", "\\xzz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decodeInput(c.in); got != c.want {
				t.Fatalf("decodeInput(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
