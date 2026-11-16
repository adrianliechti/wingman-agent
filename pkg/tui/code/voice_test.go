package code

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/audio"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

type voiceTestAgent struct {
	*uiTestAgent
	tr *audio.Transcriber
}

func (a *voiceTestAgent) Transcriber(string) (*audio.Transcriber, bool) { return a.tr, a.tr != nil }

type voiceTest struct {
	app      *App
	now      time.Time
	requests atomic.Int32
	stopped  atomic.Bool
	sample   int16
	chunks   int
}

func newVoiceTest(t *testing.T, transcript string) *voiceTest {
	t.Helper()
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")

	vt := &voiceTest{now: time.Unix(1000, 0), sample: 4000, chunks: 20}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vt.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"text":"`+transcript+`"}`)
	}))
	t.Cleanup(srv.Close)

	client := openai.NewClient(option.WithBaseURL(srv.URL+"/v1"), option.WithAPIKey("test"), option.WithMaxRetries(0))
	agent := &voiceTestAgent{uiTestAgent: newUITestAgent(nil), tr: &audio.Transcriber{Client: client, Model: "gpt-transcribe"}}
	vt.app = &App{ctx: context.Background(), queue: make(chan func(), 64), agent: agent, editor: NewEditor()}
	vt.app.voice.clock = func() time.Time { return vt.now }
	vt.app.voice.capture = func(ctx context.Context, w io.Writer) error {
		chunk := make([]byte, audio.SampleRate*2/10)
		for i := 0; i < len(chunk); i += 2 {
			binary.LittleEndian.PutUint16(chunk[i:], uint16(vt.sample))
		}
		for range vt.chunks {
			w.Write(chunk)
		}
		<-ctx.Done()
		vt.stopped.Store(true)
		return nil
	}
	return vt
}

func (vt *voiceTest) key(after time.Duration, r rune) {
	vt.now = vt.now.Add(after)
	vt.app.handleKey(inline.KeyEvent{Key: inline.KeyRune, Rune: r})
}

// hold replays a held Space: the press, the OS repeat delay, then rapid repeats.
func (vt *voiceTest) hold(repeats int) {
	vt.key(time.Second, ' ')
	vt.key(400*time.Millisecond, ' ')
	for range repeats {
		vt.key(30*time.Millisecond, ' ')
	}
}

func (vt *voiceTest) release(t *testing.T) {
	t.Helper()
	vt.now = vt.now.Add(time.Second)
	vt.app.checkVoiceRelease(vt.app.voice.session)
	vt.drain(t)
}

func (vt *voiceTest) drain(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for vt.app.voice.session != nil {
		select {
		case fn := <-vt.app.queue:
			fn()
		case <-deadline:
			t.Fatal("voice input did not finish")
		}
	}
}

func (vt *voiceTest) text() string { return vt.app.editor.Text() }

func TestVoiceEmptyPromptRecordsOnFirstSpace(t *testing.T) {
	vt := newVoiceTest(t, "run the tests")

	vt.key(time.Second, ' ')
	if vt.app.voice.session == nil {
		t.Fatal("space on an empty prompt did not start recording")
	}
	if vt.text() != "" {
		t.Fatalf("space on an empty prompt typed %q", vt.text())
	}
	if status := vt.app.voiceStatus(); !strings.Contains(status, "Listening") {
		t.Fatalf("footer = %q, want listening status", status)
	}

	vt.key(400*time.Millisecond, ' ')
	for range 20 {
		vt.key(30*time.Millisecond, ' ')
	}
	vt.release(t)
	if vt.text() != "run the tests" {
		t.Fatalf("editor = %q", vt.text())
	}
}

func TestVoiceEmptyPromptTapIsDropped(t *testing.T) {
	vt := newVoiceTest(t, "unused")
	vt.chunks = 8

	vt.key(time.Second, ' ')
	vt.now = vt.now.Add(voiceStartGrace)
	vt.app.checkVoiceRelease(vt.app.voice.session)
	vt.drain(t)
	if vt.text() != "" || vt.app.toast != nil || vt.requests.Load() != 0 {
		t.Fatalf("tap left editor %q, toast %+v, %d requests", vt.text(), vt.app.toast, vt.requests.Load())
	}
}

func TestVoiceStartGraceWaitsForKeyRepeat(t *testing.T) {
	vt := newVoiceTest(t, "unused")
	vt.key(time.Second, ' ')
	s := vt.app.voice.session

	vt.now = vt.now.Add(voiceStartGrace - time.Millisecond)
	vt.app.checkVoiceRelease(s)
	if vt.stopped.Load() || vt.app.voice.session != s {
		t.Fatal("recording stopped before key repeat could begin")
	}
	vt.app.endVoice()
}

func TestVoiceHoldAfterTextKeepsSeparator(t *testing.T) {
	vt := newVoiceTest(t, "run the tests")
	vt.app.editor.SetText("then")

	vt.key(time.Second, ' ')
	vt.key(400*time.Millisecond, ' ')
	if vt.text() != "then " || vt.app.voice.session != nil {
		t.Fatalf("after first repeat: editor %q, recording %v", vt.text(), vt.app.voice.session != nil)
	}
	vt.key(30*time.Millisecond, ' ')
	if vt.app.voice.session == nil {
		t.Fatal("rapid key repeat did not start recording")
	}
	for range 20 {
		vt.key(30*time.Millisecond, ' ')
	}
	if vt.text() != "then " {
		t.Fatalf("repeats typed into the prompt: %q", vt.text())
	}

	vt.release(t)
	if vt.text() != "then run the tests" {
		t.Fatalf("editor = %q", vt.text())
	}
	if vt.requests.Load() != 1 {
		t.Fatalf("transcription requests = %d, want 1", vt.requests.Load())
	}
}

func TestVoiceHoldAfterSpaceDoesNotDoubleIt(t *testing.T) {
	vt := newVoiceTest(t, "tests")
	vt.app.editor.SetText("run the ")

	vt.hold(3)
	vt.release(t)
	if vt.text() != "run the tests" {
		t.Fatalf("editor = %q", vt.text())
	}
}

func TestVoiceInsertsAtCursorWithSeparators(t *testing.T) {
	vt := newVoiceTest(t, "quickly")
	vt.app.editor.SetText("fix bug")
	vt.app.editor.cursor = 3

	vt.hold(3)
	vt.release(t)
	if vt.text() != "fix quickly bug" {
		t.Fatalf("editor = %q", vt.text())
	}

	vt = newVoiceTest(t, "please")
	vt.app.editor.SetText("fix it")
	vt.app.editor.cursor = 0
	vt.hold(3)
	vt.release(t)
	if vt.text() != "please fix it" {
		t.Fatalf("editor = %q", vt.text())
	}
}

func TestVoiceTypedSpacesStayText(t *testing.T) {
	for _, gap := range []time.Duration{200 * time.Millisecond, 130 * time.Millisecond} {
		vt := newVoiceTest(t, "unused")
		vt.app.editor.SetText("a")
		for range 8 {
			vt.key(gap, ' ')
		}
		if vt.app.voice.session != nil {
			t.Fatalf("spaces typed %s apart started recording", gap)
		}
		vt.key(gap, 'b')
		if want := "a" + strings.Repeat(" ", 8) + "b"; vt.text() != want {
			t.Fatalf("editor = %q, want %q", vt.text(), want)
		}
	}
}

func TestVoiceDoubleSpaceIsTypedAfterPause(t *testing.T) {
	vt := newVoiceTest(t, "unused")
	vt.app.editor.SetText("a")
	vt.key(time.Second, ' ')
	vt.key(300*time.Millisecond, ' ')
	if vt.text() != "a " {
		t.Fatalf("second space typed before the hold was ruled out: %q", vt.text())
	}

	vt.now = vt.now.Add(time.Second)
	deadline := time.After(2 * time.Second)
	for vt.text() != "a  " {
		select {
		case fn := <-vt.app.queue:
			fn()
		case <-deadline:
			t.Fatalf("second space never typed: %q", vt.text())
		}
	}
}

func TestVoiceEscCancelsWithoutTranscribing(t *testing.T) {
	vt := newVoiceTest(t, "unused")
	vt.app.editor.SetText("draft")

	vt.hold(3)
	vt.app.handleKey(inline.KeyEvent{Key: inline.KeyEsc})
	if vt.app.voice.session != nil {
		t.Fatal("Esc did not end voice input")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !vt.stopped.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !vt.stopped.Load() {
		t.Fatal("Esc did not stop the microphone")
	}
	if vt.text() != "draft " {
		t.Fatalf("editor = %q", vt.text())
	}
	if vt.requests.Load() != 0 {
		t.Fatal("cancelled recording was transcribed")
	}
}

func TestVoiceOtherKeyEndsRecording(t *testing.T) {
	vt := newVoiceTest(t, "hello")

	vt.key(time.Second, ' ')
	vt.key(500*time.Millisecond, 'x')
	vt.drain(t)
	if vt.text() != "hello" {
		t.Fatalf("editor = %q, want only the transcript", vt.text())
	}
}

func TestVoiceWithoutTranscriberTypesSpaces(t *testing.T) {
	vt := newVoiceTest(t, "unused")
	vt.app.agent = newUITestAgent(nil)

	vt.hold(5)
	if vt.app.voice.session != nil || vt.app.voice.pending {
		t.Fatal("voice input engaged without a transcription model")
	}
	if got := len(vt.text()); got != 7 {
		t.Fatalf("typed %d spaces, want 7", got)
	}
}

func TestVoiceSilentMicrophoneReportsNoAudio(t *testing.T) {
	vt := newVoiceTest(t, "unused")
	vt.sample = 0

	vt.hold(3)
	vt.release(t)
	if vt.app.toast == nil || !strings.Contains(vt.app.toast.message, "No audio detected") {
		t.Fatalf("toast = %+v, want no audio warning", vt.app.toast)
	}
	if vt.requests.Load() != 0 {
		t.Fatal("silence was sent for transcription")
	}
}

func TestVoiceMeter(t *testing.T) {
	if got := voiceMeter(0); got != "▁" {
		t.Errorf("silence meter = %q", got)
	}
	if got := voiceMeter(1); got != "█" {
		t.Errorf("full scale meter = %q", got)
	}
}
