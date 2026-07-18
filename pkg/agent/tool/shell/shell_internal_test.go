package shell

import (
	"strings"
	"testing"
)

func TestCappedBufferDropsBeyondLimit(t *testing.T) {
	var b cappedBuffer

	chunk := strings.Repeat("x", 1024*1024)
	for range 20 {
		n, err := b.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("write returned %d, %v", n, err)
		}
	}

	result := b.result()
	if b.buf.Len() != maxOutputBytes {
		t.Fatalf("buffer holds %d bytes, want %d", b.buf.Len(), maxOutputBytes)
	}
	if b.dropped != 4*1024*1024 {
		t.Fatalf("dropped %d bytes, want %d", b.dropped, 4*1024*1024)
	}
	if !strings.Contains(result, "[output capped at 16MB; 4194304 further bytes dropped]") {
		t.Fatalf("missing cap notice, got tail: %q", result[len(result)-100:])
	}
}

func TestCappedBufferSmallOutputUntouched(t *testing.T) {
	var b cappedBuffer
	b.Write([]byte("hello"))

	if got := b.result(); got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressBufferReportsLastCompleteLine(t *testing.T) {
	var reported []string
	b := &progressBuffer{report: func(line string) { reported = append(reported, line) }}

	b.Write([]byte("compiling foo.go\ncompiling ba"))
	b.Write([]byte("r.go\n"))

	if len(reported) == 0 {
		t.Fatal("expected a progress report")
	}
	if got := reported[0]; got != "compiling foo.go" {
		t.Fatalf("first report = %q", got)
	}

	if got := b.result(); got != "compiling foo.go\ncompiling bar.go\n" {
		t.Fatalf("result = %q", got)
	}
}

func TestProgressBufferSkipsBlankLines(t *testing.T) {
	var reported []string
	b := &progressBuffer{report: func(line string) { reported = append(reported, line) }}

	b.Write([]byte("real output\n\n   \n"))

	if len(reported) != 1 || reported[0] != "real output" {
		t.Fatalf("reported = %v", reported)
	}
}

func TestProgressBufferNilReport(t *testing.T) {
	b := &progressBuffer{}
	b.Write([]byte("output\n"))

	if got := b.result(); got != "output\n" {
		t.Fatalf("result = %q", got)
	}
}
