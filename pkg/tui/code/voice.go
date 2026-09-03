package code

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

const maxVoiceRecordingDuration = 2 * time.Minute

func (a *App) voiceContext() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *App) discoverVoice() {
	if a.voice == nil {
		return
	}
	go func() {
		discoveryCtx, cancel := context.WithTimeout(a.voiceContext(), 15*time.Second)
		capability, err := a.voice.Discover(discoveryCtx)
		cancel()
		a.post(func() {
			a.voiceChecked = true
			if err == nil {
				a.voiceModel = capability.Model
			}
			a.refreshCommandCenter()
			a.invalidate()
		})
	}()
}

func (a *App) voiceReady() bool {
	return a.voice != nil && a.voiceModel != "" && a.voiceRecorder != nil
}

func (a *App) toggleVoice() {
	if a.voiceRecording != nil {
		a.stopVoiceRecording()
		return
	}
	if a.voiceTranscribing {
		a.showToast("Voice transcription is still running", theme.Default.Yellow)
		return
	}
	if a.voice == nil {
		a.showToast("Voice input is unavailable", theme.Default.Yellow)
		return
	}
	if !a.voiceChecked {
		a.showToast("Still checking voice availability", theme.Default.Yellow)
		return
	}
	if a.voiceModel == "" {
		a.showToast("No compatible transcription model was found", theme.Default.Yellow)
		return
	}
	if a.voiceRecorder == nil {
		message := "Voice input needs ffmpeg, SoX rec, or arecord"
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			message = "TUI voice recording is supported on macOS and Linux"
		}
		a.showToast(message, theme.Default.Yellow)
		return
	}

	recording, err := a.voiceRecorder.Start(a.voiceContext())
	if err != nil {
		a.showToast(fmt.Sprintf("Could not start voice input: %v", err), theme.Default.Red)
		return
	}
	a.voiceRecording = recording
	a.voiceRecordingSeq++
	sequence := a.voiceRecordingSeq
	a.invalidate()
	a.voiceRecordingTimer = time.AfterFunc(maxVoiceRecordingDuration, func() {
		a.post(func() {
			if a.voiceRecording != nil && a.voiceRecordingSeq == sequence {
				a.stopVoiceRecording()
			}
		})
	})
}

func (a *App) cancelVoiceRecording() {
	recording := a.voiceRecording
	if recording == nil {
		return
	}
	a.voiceRecording = nil
	if a.voiceRecordingTimer != nil {
		a.voiceRecordingTimer.Stop()
		a.voiceRecordingTimer = nil
	}
	go func() {
		_ = recording.Cancel()
	}()
	a.showToast("Voice recording cancelled", theme.Default.Yellow)
}

func (a *App) stopVoiceRecording() {
	recording := a.voiceRecording
	if recording == nil {
		return
	}
	a.voiceRecording = nil
	if a.voiceRecordingTimer != nil {
		a.voiceRecordingTimer.Stop()
		a.voiceRecordingTimer = nil
	}
	a.voiceTranscribing = true
	a.voiceInsertPosition = a.editor.cursor
	operationCtx, cancelOperation := context.WithCancel(a.voiceContext())
	a.voiceCancel = cancelOperation
	a.invalidate()

	go func() {
		defer cancelOperation()
		stopCtx, cancel := context.WithTimeout(operationCtx, 5*time.Second)
		clip, err := recording.Stop(stopCtx)
		cancel()
		if err != nil {
			a.finishVoiceTranscription("", fmt.Errorf("finish recording: %w", err))
			return
		}
		defer clip.Remove()

		file, err := os.Open(clip.Path)
		if err != nil {
			a.finishVoiceTranscription("", fmt.Errorf("open recording: %w", err))
			return
		}
		text, err := a.voice.Transcribe(operationCtx, clip.Filename, clip.ContentType, file)
		_ = file.Close()
		a.finishVoiceTranscription(text, err)
	}()
}

func (a *App) finishVoiceTranscription(text string, err error) {
	a.post(func() {
		a.voiceTranscribing = false
		a.voiceCancel = nil
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				a.showToast(fmt.Sprintf("Voice input failed: %v", err), theme.Default.Red)
			}
			return
		}
		if strings.TrimSpace(text) == "" {
			a.showToast("No speech was detected", theme.Default.Yellow)
			return
		}
		insertVoiceText(a.editor, a.voiceInsertPosition, text)
		a.syncCommandPopup()
		a.showToast("Voice transcription inserted", theme.Default.Green)
	})
}

func insertVoiceText(editor *Editor, position int, transcript string) {
	spoken := []rune(strings.TrimSpace(transcript))
	if len(spoken) == 0 {
		return
	}
	position = max(0, min(position, len(editor.value)))
	insert := make([]rune, 0, len(spoken)+2)
	if position > 0 && !unicode.IsSpace(editor.value[position-1]) {
		insert = append(insert, ' ')
	}
	insert = append(insert, spoken...)
	if position < len(editor.value) && !unicode.IsSpace(editor.value[position]) {
		insert = append(insert, ' ')
	}
	editor.ReplaceRange(position, position, string(insert))
}
