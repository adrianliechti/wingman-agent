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
