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
