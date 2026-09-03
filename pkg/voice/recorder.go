package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

type AudioClip struct {
	Path        string
	Filename    string
	ContentType string
}

func (c AudioClip) Remove() error {
	if c.Path == "" {
		return nil
	}
	return os.Remove(c.Path)
}

type Recording interface {
	Stop(context.Context) (AudioClip, error)
	Cancel() error
}

type Recorder interface {
	Name() string
	Start(context.Context) (Recording, error)
}

type recorderSpec struct {
	name string
	path string
	args func(string) []string
}

type commandRecorder struct {
	spec recorderSpec
}

// DetectRecorder finds a small, established system recorder. Recording is
// intentionally unavailable when none is installed instead of presenting a
// TUI control that cannot capture audio.
func DetectRecorder() Recorder {
	for _, candidate := range recorderCandidates() {
		path, err := exec.LookPath(candidate.name)
		if err != nil {
			continue
		}
		candidate.path = path
		return &commandRecorder{spec: candidate}
	}
	return nil
}

func recorderCandidates() []recorderSpec {
	rec := recorderSpec{name: "rec", args: func(path string) []string {
		return []string{"--clobber", "-q", "-c", "1", "-r", "16000", "-b", "16", "-e", "signed-integer", path}
	}}
	ffmpeg := func(format, input string) recorderSpec {
		return recorderSpec{name: "ffmpeg", args: func(path string) []string {
			return []string{
				"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
				"-f", format, "-i", input, "-ac", "1", "-ar", "16000",
				"-c:a", "pcm_s16le", path,
			}
		}}
	}

	switch runtime.GOOS {
	case "darwin":
		return []recorderSpec{rec, ffmpeg("avfoundation", ":0")}
	case "linux":
		arecord := recorderSpec{name: "arecord", args: func(path string) []string {
			return []string{"-q", "-f", "S16_LE", "-r", "16000", "-c", "1", path}
		}}
		return []recorderSpec{arecord, rec, ffmpeg("pulse", "default")}
	default:
		return nil
	}
}

func (r *commandRecorder) Name() string { return r.spec.name }

func (r *commandRecorder) Start(ctx context.Context) (Recording, error) {
	file, err := os.CreateTemp("", "wingman-voice-*.wav")
	if err != nil {
		return nil, fmt.Errorf("create temporary recording: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close temporary recording: %w", err)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.spec.path, r.spec.args(path)...)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("start %s: %w", r.spec.name, err)
	}

	recording := &commandRecording{
		cmd:    cmd,
		clip:   AudioClip{Path: path, Filename: filepath.Base(path), ContentType: "audio/wav"},
		stderr: &stderr,
		done:   make(chan struct{}),
	}
	go func() {
		recording.waitErr = cmd.Wait()
		close(recording.done)
	}()
	return recording, nil
}

type commandRecording struct {
	cmd    *exec.Cmd
	clip   AudioClip
	stderr *bytes.Buffer
	done   chan struct{}

	stopOnce sync.Once
	waitErr  error
}

func (r *commandRecording) Stop(ctx context.Context) (AudioClip, error) {
	r.stopOnce.Do(func() {
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Signal(os.Interrupt)
		}
	})

	select {
	case <-r.done:
	case <-ctx.Done():
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		<-r.done
		_ = r.clip.Remove()
		return AudioClip{}, ctx.Err()
	}

	info, statErr := os.Stat(r.clip.Path)
	if statErr == nil && info.Size() > 44 {
		// Recorders commonly report an interrupted exit after finalizing a WAV;
		// the finalized file is the authoritative success signal.
		return r.clip, nil
	}
	_ = r.clip.Remove()
	if message := string(bytes.TrimSpace(r.stderr.Bytes())); message != "" {
		return AudioClip{}, errors.New(message)
	}
	if r.waitErr != nil {
		return AudioClip{}, r.waitErr
	}
	if statErr != nil {
		return AudioClip{}, statErr
	}
	return AudioClip{}, errors.New("recorder produced no audio")
}

func (r *commandRecording) Cancel() error {
	r.stopOnce.Do(func() {
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
	})
	<-r.done
	return r.clip.Remove()
}
