package fs

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type windowReader func([]byte) (int, error)

func (r windowReader) Read(p []byte) (int, error) { return r(p) }

func TestReadWindowStopsWhenCancelledDuringScan(t *testing.T) {
	for _, scenario := range []struct {
		name, content string
		offset        int
	}{
		{"skipping lines", strings.Repeat("line\n", 100_000), 90_000},
		{"final requested line", strings.Repeat("x", 70*1024) + "\nremainder\n", 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			source := strings.NewReader(scenario.content)
			reads := 0
			reader := windowReader(func(p []byte) (int, error) {
				reads++
				if reads == 2 {
					cancel()
				}
				return source.Read(p)
			})
			output, err := readTextWindow(ctx, reader, "large.txt", int64(len(scenario.content)), scenario.offset, 1)
			if !errors.Is(err, context.Canceled) || output != "" {
				t.Fatalf("cancelled scan returned %q, %v", output, err)
			}
			if reads != 2 {
				t.Fatalf("scan kept reading after cancellation: %d reads", reads)
			}
		})
	}
}

func TestReadWindowDoesNotStartAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reader := windowReader(func([]byte) (int, error) {
		t.Fatal("cancelled window tried to read")
		return 0, nil
	})
	_, err := readTextWindow(ctx, reader, "large.txt", 1_000_000, 1, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read error = %v", err)
	}
}

func TestReadWindowRejectsBinaryWithinSniffWindow(t *testing.T) {
	for _, content := range []string{"hi\x00", strings.Repeat("x", 7000) + "\x00\n"} {
		if _, err := readTextWindow(t.Context(), strings.NewReader(content), "data.bin", int64(len(content)), 1, 1); err == nil || !strings.Contains(err.Error(), "binary") {
			t.Fatalf("binary read error = %v", err)
		}
	}
}

func TestReadWindowMatchesTextLineEndings(t *testing.T) {
	for _, content := range []string{"one\rtwo\rthree", "one\r\ntwo\rthree\n", "\ufeffone\rtwo\r"} {
		for offset := 1; offset <= 4; offset++ {
			got, err := readTextWindow(t.Context(), strings.NewReader(content), "text", int64(len(content)), offset, 1)
			if err != nil {
				t.Fatal(err)
			}
			got, _, _ = strings.Cut(got, "\n\n[Showing")
			want, _, _ := strings.Cut(formatRead([]byte(content), offset, 1), "\n\n[Showing")
			if got != want {
				t.Errorf("content=%q offset=%d: got %q, want %q", content, offset, got, want)
			}
		}
	}
}

func TestReadWindowBoundsEmptyLines(t *testing.T) {
	content := strings.Repeat("\n", 1_000_000)
	source := strings.NewReader(content)
	reader := windowReader(func(p []byte) (int, error) { return source.Read(p) })
	if _, err := readTextWindow(t.Context(), reader, "empty-lines", int64(len(content)), 1, 1_000_000); err != nil {
		t.Fatal(err)
	}
	if consumed := len(content) - source.Len(); consumed > 128*1024 {
		t.Fatalf("read %d source bytes despite the %d-byte output budget", consumed, DefaultMaxBytes)
	}
}

func TestReadContentBoundsGrowingSources(t *testing.T) {
	const limit = 1024
	readBytes := 0
	reader := windowReader(func(p []byte) (int, error) {
		readBytes += len(p)
		if readBytes > limit+1 {
			t.Fatalf("read continued beyond limit: %d bytes", readBytes)
		}
		return len(p), nil // Simulate a source that keeps growing.
	})
	content, err := readContent(t.Context(), reader, limit)
	if err != nil || len(content) != limit+1 {
		t.Fatalf("bounded read = %d bytes, %v", len(content), err)
	}
}

func TestReadContentStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := 0
	reader := windowReader(func(p []byte) (int, error) {
		reads++
		cancel()
		return len(p), nil
	})
	_, err := readContent(ctx, reader, -1)
	if !errors.Is(err, context.Canceled) || reads != 1 {
		t.Fatalf("cancelled read: reads=%d error=%v", reads, err)
	}
}
