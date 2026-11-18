package code

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/audio"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type conversationUITestAgent struct {
	*uiTestAgent
	model *audio.Realtime
}

func (a *conversationUITestAgent) Realtime(string) (*audio.Realtime, bool) { return a.model, true }

func TestConversationTerminalCleanup(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	for _, end := range []string{"escape", "switch session", "headphone interruption", "next turn after playback", "queued replies", "upstream failure"} {
		t.Run(end, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			closed := make(chan struct{})
			peers := make(chan *websocket.Conn, 1)
			forwarded := make(chan struct{}, 8)
			truncations := make(chan int, 8)
			replies := make(chan struct{}, 8)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				defer close(closed)
				peers <- conn
				for {
					var event struct {
						Type       string
						AudioEndMS int `json:"audio_end_ms"`
					}
					if wsjson.Read(ctx, conn, &event) != nil {
						return
					}
					if event.Type == "session.update" {
						_ = wsjson.Write(ctx, conn, map[string]any{"type": "session.updated"})
					}
					if event.Type == "input_audio_buffer.append" {
						forwarded <- struct{}{}
					}
					if event.Type == "conversation.item.truncate" {
						truncations <- event.AudioEndMS
					}
					if event.Type == "response.create" {
						_ = wsjson.Write(ctx, conn, map[string]any{"type": "response.created"})
						replies <- struct{}{}
					}
				}
			}))
			defer gateway.Close()
			agent := &conversationUITestAgent{uiTestAgent: newUITestAgent(nil), model: &audio.Realtime{Client: openai.NewClient(option.WithBaseURL(gateway.URL+"/v1"), option.WithAPIKey("test")), Model: "gpt-realtime-test"}}
			app := &App{ctx: ctx, agent: agent, editor: NewEditor(), queue: make(chan func(), 64), sessionID: "old"}
			app.turns = code.NewTurnManager(ctx, agent, nil)
			defer app.turns.Close()
			capturing, stopped := make(chan struct{}), make(chan struct{})
			captureChunk := make(chan struct{}, 1)
			captured := make(chan struct{}, 1)
			playing, playbackStopped := make(chan struct{}, 4), make(chan struct{}, 4)
			finishPlayback := make(chan struct{}, 4)
			app.voice.capture = func(ctx context.Context, w io.Writer) error {
				close(capturing)
				_, _ = w.Write(make([]byte, 3200))
				defer close(stopped)
				for {
					select {
					case <-captureChunk:
						if _, err := w.Write(make([]byte, 3200)); err != nil {
							return err
						}
						captured <- struct{}{}
					case <-ctx.Done():
						return nil
					}
				}
			}
			app.voice.play = func(ctx context.Context, pcm []byte) error {
				playing <- struct{}{}
				defer func() { playbackStopped <- struct{}{} }()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-finishPlayback:
					return nil
				}
			}
			if end == "headphone interruption" {
				command := app.findBuiltin("/voice headphone")
				if command == nil || command.RunArgs == nil {
					t.Fatal("/voice headphone is not registered")
				}
				command.RunArgs(app, "headphone")
			} else {
				app.toggleConversation()
			}
			defer app.endConversation()
			select {
			case <-capturing:
			case <-ctx.Done():
				t.Fatal("voice did not start")
			}
			peer := <-peers
			select {
			case <-forwarded:
			case <-ctx.Done():
				t.Fatal("microphone audio was not forwarded")
			}
			// Voice only speaks in a reply it asked for, here the one to a spoken request.
			for _, kind := range []string{"input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped"} {
				if err := wsjson.Write(ctx, peer, map[string]any{"type": kind}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-replies:
			case <-ctx.Done():
				t.Fatal("voice did not reply to speech")
			}
			for _, event := range []map[string]any{
				{"type": "response.output_audio.delta", "item_id": "reply", "delta": base64.StdEncoding.EncodeToString(make([]byte, 48000))},
				{"type": "response.output_audio.done", "item_id": "reply"},
			} {
				if err := wsjson.Write(ctx, peer, event); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-playing:
			case <-ctx.Done():
				t.Fatal("playback did not start")
			}
			// Only headphone mode forwards microphone input during playback.
			captureChunk <- struct{}{}
			select {
			case <-captured:
			case <-ctx.Done():
				t.Fatal("microphone capture stopped during playback")
			}
			if end == "headphone interruption" {
				select {
				case <-forwarded:
				case <-ctx.Done():
					t.Fatal("headphone mode muted the microphone during playback")
				}
			} else {
				select {
				case <-forwarded:
					t.Fatal("microphone forwarded speaker audio during playback")
				case <-time.After(30 * time.Millisecond):
				}
			}
			playbackAlreadyStopped := false
			if end == "queued replies" || end == "headphone interruption" {
				for _, event := range []map[string]any{
					{"type": "response.output_audio.delta", "item_id": "next", "delta": base64.StdEncoding.EncodeToString(make([]byte, 48000))},
					{"type": "response.output_audio.done", "item_id": "next"},
				} {
					if err := wsjson.Write(ctx, peer, event); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-playbackStopped:
					t.Fatal("new reply cut off the current answer")
				case <-time.After(30 * time.Millisecond):
				}
			}
			switch end {
			case "upstream failure":
				peer.CloseNow()
				select {
				case fn := <-app.queue:
					fn()
				case <-ctx.Done():
					t.Fatal("upstream failure did not end voice")
				}
				if app.toast == nil || !strings.Contains(app.toast.message, "Voice conversation ended:") {
					t.Fatalf("upstream failure disappeared silently: %+v", app.toast)
				}
			case "escape":
				app.handleKey(inline.KeyEvent{Key: inline.KeyEsc})
			case "switch session":
				app.activateSession("new")
			case "headphone interruption":
				if err := wsjson.Write(ctx, peer, map[string]any{"type": "input_audio_buffer.speech_started"}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-playbackStopped:
					playbackAlreadyStopped = true
				case <-ctx.Done():
					t.Fatal("speech did not interrupt playback")
				}
				if app.conversation == nil {
					t.Fatal("interrupting an answer ended the conversation")
				}
				select {
				case played := <-truncations:
					if played < 0 || played >= 1000 {
						t.Fatalf("interrupted playback position = %d", played)
					}
				case <-ctx.Done():
					t.Fatal("interrupted answer was not truncated")
				}
				select {
				case played := <-truncations:
					if played != 0 {
						t.Fatalf("unheard queued answer was truncated to %d ms", played)
					}
				case <-ctx.Done():
					t.Fatal("queued answer was not discarded on interruption")
				}
				select {
				case <-playing:
					t.Fatal("queued answer resumed after interruption")
				case <-time.After(30 * time.Millisecond):
				}
				app.endConversation()
			case "next turn after playback":
				finishPlayback <- struct{}{}
				for app.conversation.playing.Load() && ctx.Err() == nil {
					time.Sleep(time.Millisecond)
				}
				if ctx.Err() != nil {
					t.Fatal("playback did not finish")
				}
				captureChunk <- struct{}{}
				select {
				case <-captured:
				case <-ctx.Done():
					t.Fatal("microphone capture stopped after playback")
				}
				select {
				case <-forwarded:
					t.Fatal("residual speaker audio was forwarded after playback")
				case <-time.After(30 * time.Millisecond):
				}
				timer := time.NewTimer(time.Until(time.Unix(0, app.conversation.listenAfter.Load())))
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					t.Fatal("microphone did not resume")
				}
				captureChunk <- struct{}{}
				select {
				case <-forwarded:
				case <-ctx.Done():
					t.Fatal("microphone was not forwarded after playback")
				}
				if err := wsjson.Write(ctx, peer, map[string]any{"type": "input_audio_buffer.speech_started"}); err != nil {
					t.Fatal(err)
				}
				select {
				case played := <-truncations:
					t.Fatalf("fully heard answer was truncated to %d ms", played)
				case <-time.After(30 * time.Millisecond):
				}
				app.endConversation()
			case "queued replies":
				finishPlayback <- struct{}{}
				select {
				case <-playbackStopped:
				case <-ctx.Done():
					t.Fatal("first answer did not finish")
				}
				select {
				case <-playing:
				case <-ctx.Done():
					t.Fatal("queued answer did not play")
				}
				app.endConversation()
			}
			if app.conversation != nil {
				t.Fatal("conversation survived ending its session")
			}
			for _, done := range []chan struct{}{stopped, closed} {
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("microphone or realtime connection did not close")
				}
			}
			if !playbackAlreadyStopped {
				select {
				case <-playbackStopped:
				case <-ctx.Done():
					t.Fatal("playback did not stop")
				}
			}
		})
	}
}

func TestConversationEscapeRespectsFocusedPopupAndQueueEdit(t *testing.T) {
	for _, focus := range []string{"picker", "queue edit"} {
		t.Run(focus, func(t *testing.T) {
			app, _ := newQueueTestApp(t, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			app.conversation = &tuiConversation{cancel: cancel}
			if focus == "picker" {
				app.popup = newPopup(popupList, "Model", []PopupItem{{ID: "model", Label: "Model"}}, nil)
			} else {
				app.runQueueCommand("edit x")
			}
			app.handleKey(inline.KeyEvent{Key: inline.KeyEsc})
			if app.conversation == nil || ctx.Err() != nil {
				t.Fatal("Escape ended voice instead of closing the focused UI")
			}
			if app.popup != nil || app.editingQueueID != "" {
				t.Fatal("Escape did not close the focused UI")
			}
			app.handleKey(inline.KeyEvent{Key: inline.KeyEsc})
			if app.conversation != nil || ctx.Err() == nil {
				t.Fatal("Escape in the composer did not end voice")
			}
		})
	}
}

func TestConversationEscapeRespectsApproval(t *testing.T) {
	app, _ := newQueueTestApp(t, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app.conversation = &tuiConversation{cancel: cancel}
	done := make(chan error, 1)
	go func() {
		_, err := app.askOptionsLocked(t.Context(), "Confirm command", "Continue?", []PopupItem{{ID: "yes", Label: "Yes"}}, false, nil)
		done <- err
	}()
	select {
	case display := <-app.queue:
		display()
	case <-time.After(time.Second):
		t.Fatal("approval did not open")
	}
	app.handleKey(inline.KeyEvent{Key: inline.KeyEsc})
	if app.conversation == nil || ctx.Err() != nil || app.popup != nil {
		t.Fatal("Escape must close approval and preserve voice")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("approval did not finish")
	}
}

// /voice is listed only where a conversation can start, so WINGMAN_DISABLE_VOICE hides it.
func TestVoiceCommandListedOnlyWhereAvailable(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	listed := func(agent code.Agent) bool {
		app := &App{agent: agent, editor: NewEditor()}
		return app.findBuiltin("/voice") != nil
	}
	plain := newUITestAgent(nil)
	voice := &conversationUITestAgent{uiTestAgent: plain, model: &audio.Realtime{}}
	if listed(plain) {
		t.Fatal("/voice listed without voice")
	}
	if !listed(voice) {
		t.Fatal("/voice missing where voice is available")
	}
	t.Setenv("SSH_TTY", "/dev/pts/1")
	if listed(voice) {
		t.Fatal("/voice listed in an SSH session")
	}
}
