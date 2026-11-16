package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Play uses the platform's audio player. A complete utterance is buffered by
// the TUI; cancellation terminates playback immediately.
func Play(ctx context.Context, pcm []byte) error {
	dir, err := os.MkdirTemp("", "wingman-voice-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "speech.wav")
	if err := os.WriteFile(path, WAVAtRate(pcm, RealtimeSampleRate), 0o600); err != nil {
		return err
	}
	var players [][]string
	switch runtime.GOOS {
	case "darwin":
		players = [][]string{{"afplay", path}}
	case "windows":
		quoted := strings.ReplaceAll(path, "'", "''")
		players = [][]string{{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "$player = New-Object System.Media.SoundPlayer '" + quoted + "'; $player.PlaySync()"}}
	default:
		players = [][]string{{"pw-play", path}, {"paplay", path}, {"aplay", "-q", path}}
	}
	for _, args := range players {
		if _, err := exec.LookPath(args[0]); err != nil {
			continue
		}
		output, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("audio playback: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return ctx.Err()
	}
	return errors.New("audio: no playback program found; install pipewire-bin, pulseaudio-utils, or alsa-utils")
}

// RealtimePCM converts Capture's 16 kHz samples to the Realtime API's 24 kHz.
// Capture emits whole 100 ms chunks; repeating this interpolation preserves
// their duration and does not buffer microphone audio across writes.
func RealtimePCM(pcm []byte) []byte {
	n := len(pcm) / 2
	if n == 0 {
		return nil
	}
	out := make([]byte, n*3)
	out = out[:len(out)&^1]
	for i := 0; i < len(out)/2; i++ {
		at := i * 2
		index, fraction := at/3, at%3
		a := int32(int16(binary.LittleEndian.Uint16(pcm[index*2:])))
		b := int32(int16(binary.LittleEndian.Uint16(pcm[min(index+1, n-1)*2:])))
		value := (a*int32(3-fraction) + b*int32(fraction)) / 3
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(value)))
	}
	return out
}
