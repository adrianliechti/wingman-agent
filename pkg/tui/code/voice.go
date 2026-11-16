package code

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/adrianliechti/wingman-agent/pkg/audio"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type voiceProvider interface {
	Transcriber(sessionID string) (*audio.Transcriber, bool)
}

// Terminals report no key release, so a held Space shows up as OS key repeat:
// a first repeat after the repeat delay, then rapid repeats until release.
const (
	voiceHoldWindow   = time.Second
	voiceRepeatGap    = 120 * time.Millisecond
	voiceStartGrace   = 800 * time.Millisecond
	voiceReleaseAfter = 300 * time.Millisecond
	voiceMaxDuration  = 2 * time.Minute
	voiceMinBytes     = audio.SampleRate * 2
	voiceMeterWidth   = 4
)

var errNoAudio = errors.New("No audio detected from microphone")

type voiceState struct {
	clock   func() time.Time
	capture func(context.Context, io.Writer) error

	// pending is set after a typed space while it may still turn into a hold;
	// spaces arriving meanwhile are swallowed until that is decided.
	pending   bool
	since     time.Time
	last      time.Time
	swallowed int
	timer     *time.Timer

	session *voiceSession
}

type voiceSession struct {
	cancel context.CancelFunc
	stop   context.CancelFunc

	started      time.Time
	lastRepeat   time.Time
	release      *time.Timer
	transcribing bool

	levels   [voiceMeterWidth]atomic.Uint64
	levelPos atomic.Uint32
}

func (a *App) voiceNow() time.Time {
	if a.voice.clock != nil {
		return a.voice.clock()
	}
	return time.Now()
}

// handleVoiceKey reports whether ev was consumed by voice input.
func (a *App) handleVoiceKey(ev inline.KeyEvent) bool {
	space := ev.Key == inline.KeyRune && ev.Rune == ' ' && !ev.Alt
	now := a.voiceNow()

	if s := a.voice.session; s != nil {
		switch {
		case ev.Key == inline.KeyEsc, ev.Key == inline.KeyCtrl && ev.Rune == 'c':
			a.endVoice()
			return true
		case s.transcribing:
			return false
		case space:
			s.lastRepeat = now
			return true
		}
		// Another key interrupts the OS key repeat, so the hold is over.
		s.stop()
		return true
	}

	if a.voice.pending {
		if space {
			gap := now.Sub(a.voice.last)
			a.voice.last = now
			a.voice.swallowed++
			if a.voice.swallowed >= 2 && gap <= voiceRepeatGap {
				a.startVoice(now, true)
			} else {
				a.armVoicePending(voiceRepeatGap + 30*time.Millisecond)
			}
			return true
		}
		a.resolveVoicePending()
	}

	if !space || a.overlay != nil || a.popup != nil || a.isShellInput() || a.voiceTranscriber() == nil {
		return false
	}
	// Leading spaces do nothing in a prompt, so there Space records right away.
	if a.editor.cursor == 0 {
		a.startVoice(now, false)
		return true
	}
	a.voice.pending, a.voice.since, a.voice.last, a.voice.swallowed = true, now, now, 0
	a.armVoicePending(voiceHoldWindow)
	return false
}

func (a *App) voiceTranscriber() *audio.Transcriber {
	provider, ok := a.agent.(voiceProvider)
	if !ok || remoteTerminal() {
		return nil
	}
	tr, _ := provider.Transcriber(a.sessionID)
	return tr
}

func (a *App) armVoicePending(after time.Duration) {
	if a.voice.timer != nil {
		a.voice.timer.Stop()
	}
	a.voice.timer = time.AfterFunc(after, func() { a.post(a.checkVoicePending) })
}

func (a *App) checkVoicePending() {
	if !a.voice.pending {
		return
	}
	now := a.voiceNow()
	if a.voice.swallowed == 0 && now.Sub(a.voice.since) < voiceHoldWindow ||
		a.voice.swallowed > 0 && now.Sub(a.voice.last) <= voiceRepeatGap {
		a.armVoicePending(voiceRepeatGap)
		return
	}
	a.resolveVoicePending()
}

// resolveVoicePending decides the typed space was not a hold and types any
// spaces swallowed while that was undecided.
func (a *App) resolveVoicePending() {
	if a.voice.timer != nil {
		a.voice.timer.Stop()
	}
	a.voice.pending = false
	if n := a.voice.swallowed; n > 0 {
		a.voice.swallowed = 0
		a.editor.Insert(strings.Repeat(" ", n))
		a.syncCommandPopup()
		a.markDraftChanged()
		a.invalidate()
	}
}

func (a *App) startVoice(now time.Time, held bool) {
	if a.voice.timer != nil {
		a.voice.timer.Stop()
	}
	a.voice.pending, a.voice.swallowed = false, 0

	tr := a.voiceTranscriber()
	if tr == nil {
		return
	}
	// The typed space separates the transcript unless it doubles an existing one.
	if c := a.editor.cursor; held && c >= 2 && a.editor.value[c-1] == ' ' && a.editor.value[c-2] == ' ' {
		a.editor.ReplaceRange(c-1, c, "")
	}

	ctx, cancel := context.WithCancel(a.ctx)
	rec, stop := context.WithTimeout(ctx, voiceMaxDuration)
	s := &voiceSession{cancel: cancel, stop: stop, started: now}
	if held {
		s.lastRepeat = now
	}
	s.release = time.AfterFunc(voiceReleaseAfter, func() {
		a.post(func() { a.checkVoiceRelease(s) })
	})
	a.voice.session = s

	capture := a.voice.capture
	if capture == nil {
		capture = audio.Capture
	}
	go a.recordVoice(ctx, rec, s, tr, capture)
	a.invalidate()
}

func (a *App) checkVoiceRelease(s *voiceSession) {
	if a.voice.session != s || s.transcribing {
		return
	}
	deadline := s.lastRepeat.Add(voiceReleaseAfter)
	if s.lastRepeat.IsZero() {
		deadline = s.started.Add(voiceStartGrace)
	}
	if now := a.voiceNow(); now.Before(deadline) {
		s.release.Reset(deadline.Sub(now))
		return
	}
	s.stop()
}

func (a *App) recordVoice(ctx, rec context.Context, s *voiceSession, tr *audio.Transcriber, capture func(context.Context, io.Writer) error) {
	var pcm bytes.Buffer
	err := capture(rec, voiceWriter(func(p []byte) {
		s.levels[s.levelPos.Add(1)%voiceMeterWidth].Store(math.Float64bits(audio.Level(p)))
		a.requestRender()
		pcm.Write(p)
	}))

	var text string
	// Shorter recordings are taps, not dictation.
	if err == nil && ctx.Err() == nil && pcm.Len() >= voiceMinBytes {
		if audio.Level(pcm.Bytes()) == 0 {
			err = errNoAudio
		} else {
			a.post(func() {
				s.transcribing = true
				a.invalidate()
			})
			text, err = tr.Transcribe(ctx, audio.WAV(pcm.Bytes()))
		}
	}
	a.post(func() { a.finishVoice(s, text, err) })
}

func (a *App) finishVoice(s *voiceSession, text string, err error) {
	if a.voice.session != s {
		return
	}
	a.endVoice()
	switch {
	case err != nil:
		a.showToast("Voice input failed: "+err.Error(), theme.Default.Red)
	case text != "":
		value, c := a.editor.value, a.editor.cursor
		if c > 0 && !unicode.IsSpace(value[c-1]) {
			text = " " + text
		}
		if c < len(value) && !unicode.IsSpace(value[c]) {
			text += " "
		}
		a.editor.Insert(text)
		a.syncCommandPopup()
		a.markDraftChanged()
	}
}

// endVoice discards any recording or transcription still in flight.
func (a *App) endVoice() {
	s := a.voice.session
	if s == nil {
		return
	}
	a.voice.session = nil
	s.release.Stop()
	s.cancel()
	a.invalidate()
}

func (a *App) voiceStatus() string {
	s := a.voice.session
	if s == nil {
		return ""
	}
	t := theme.Default
	if s.transcribing {
		return colored(t.Cyan, "Transcribing…") + dim(" · esc cancels")
	}
	var meter strings.Builder
	pos := s.levelPos.Load()
	for i := range uint32(voiceMeterWidth) {
		meter.WriteString(voiceMeter(math.Float64frombits(s.levels[(pos+1+i)%voiceMeterWidth].Load())))
	}
	return colored(t.Red, meter.String()) + " " + colored(t.Foreground, "Listening") + dim(" · esc cancels")
}

func voiceMeter(level float64) string {
	steps := []rune("▁▂▃▄▅▆▇█")
	db := 20 * math.Log10(max(level, 1e-6))
	i := int((db + 60) / 50 * float64(len(steps)))
	return string(steps[min(max(i, 0), len(steps)-1)])
}

func remoteTerminal() bool {
	return os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != ""
}

type voiceWriter func([]byte)

func (w voiceWriter) Write(p []byte) (int, error) {
	w(p)
	return len(p), nil
}
