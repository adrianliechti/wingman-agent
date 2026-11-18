package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var recorders = [][]string{
	{"pw-record", "--raw", "--rate", strconv.Itoa(SampleRate), "--channels", "1", "--format", "s16", "-"},
	{"parecord", "--raw", "--rate=" + strconv.Itoa(SampleRate), "--channels=1", "--format=s16le"},
	{"arecord", "-q", "-t", "raw", "-f", "S16_LE", "-r", strconv.Itoa(SampleRate), "-c", "1"},
}

// Capture records the default microphone as PCM16LE mono at SampleRate and
// writes it to w in chunks of whole samples until ctx is done.
func Capture(ctx context.Context, w io.Writer) error {
	for _, args := range recorders {
		if path, err := exec.LookPath(args[0]); err == nil {
			return record(ctx, w, path, args[1:])
		}
	}
	return errors.New("audio: no recorder found; install pipewire-bin, pulseaudio-utils, or alsa-utils")
}

func record(ctx context.Context, w io.Writer, path string, args []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stderr = &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var writeErr error
	buf := make([]byte, chunkBytes)
	for {
		n, err := io.ReadFull(out, buf)
		if n &^= 1; n > 0 && writeErr == nil {
			if _, writeErr = w.Write(buf[:n]); writeErr != nil {
				cancel()
			}
		}
		if err != nil {
			break
		}
	}

	waitErr := cmd.Wait()
	switch {
	case writeErr != nil:
		return writeErr
	case ctx.Err() != nil:
		return nil
	case waitErr != nil:
		return fmt.Errorf("audio: %s: %w: %s", filepath.Base(path), waitErr, strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("audio: %s stopped unexpectedly", filepath.Base(path))
}
