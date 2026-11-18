package audio

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeRecorder(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestCaptureStreamsUntilCancelled(t *testing.T) {
	fakeRecorder(t, "arecord", "exec /bin/cat /dev/zero")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var chunks [][]byte
	w := writerFunc(func(p []byte) (int, error) {
		chunks = append(chunks, bytes.Clone(p))
		return len(p), nil
	})
	if err := Capture(ctx, w); err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("no audio written")
	}
	for i, c := range chunks {
		if len(c)%2 != 0 || len(c) > chunkBytes {
			t.Fatalf("chunk %d has %d bytes", i, len(c))
		}
	}
}

func TestCaptureTrimsPartialSample(t *testing.T) {
	fakeRecorder(t, "parecord", "printf abc; exec /bin/sleep 10")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	if err := Capture(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ab" {
		t.Fatalf("captured %q, want %q", out.String(), "ab")
	}
}

func TestCaptureRecorderFailure(t *testing.T) {
	fakeRecorder(t, "pw-record", "echo 'no capture device' >&2; exit 1")
	err := Capture(context.Background(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no capture device") {
		t.Fatalf("err = %v, want recorder stderr", err)
	}
}

func TestCaptureNoRecorder(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := Capture(context.Background(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no recorder found") {
		t.Fatalf("err = %v, want missing recorder error", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
