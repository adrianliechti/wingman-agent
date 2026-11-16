package code

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/audio"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type realtimeProvider interface {
	Realtime(string) (*audio.Realtime, bool)
}

type tuiConversation struct {
	cancel      context.CancelFunc
	headphone   bool
	ready       atomic.Bool
	playing     atomic.Bool
	listenAfter atomic.Int64
	level       atomic.Uint64
}

func (a *App) toggleConversation() {
	a.runVoiceCommand("")
}

func (a *App) runVoiceCommand(args string) {
	args = strings.TrimSpace(args)
	if args != "" && args != "headphone" {
		a.showToast("Usage: /voice [headphone]", theme.Default.Yellow)
		return
	}
	if a.conversation != nil {
		a.endConversation()
		return
	}
	provider, ok := a.agent.(realtimeProvider)
	if !ok || remoteTerminal() {
		a.showToast("Voice conversation requires a local terminal and a realtime model", theme.Default.Yellow)
		return
	}
	model, ok := provider.Realtime(a.sessionID)
	if !ok {
		a.showToast("No realtime model available; set WINGMAN_MODEL_REALTIME", theme.Default.Yellow)
		return
	}
	a.resolveVoicePending()
	a.endVoice()
	ctx, cancel := context.WithCancel(a.ctx)
	s := &tuiConversation{cancel: cancel, headphone: args == "headphone"}
	a.conversation = s
	sid := a.sessionID
	go func() {
		err := a.runConversation(ctx, s, model, sid)
		a.post(func() {
			if a.conversation != s {
				return
			}
			a.endConversation()
			if err != nil && !errors.Is(err, context.Canceled) {
				a.showToast("Voice conversation ended: "+err.Error(), theme.Default.Red)
			}
		})
	}()
	a.invalidate()
}

func (a *App) endConversation() {
	if s := a.conversation; s != nil {
		a.conversation = nil
		s.cancel()
		a.invalidate()
	}
}

func (a *App) runConversation(ctx context.Context, s *tuiConversation, model *audio.Realtime, sid string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan code.ConversationEvent, 128)
	voice, err := code.StartConversation(ctx, model, a.agent, a.turns, sid, func(event code.ConversationEvent) error {
		select {
		case events <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		return err
	}
	defer voice.Close()
	failures := make(chan error, 2)
	go func() {
		if ctx.Err() != nil {
			return
		}
		capture := a.voice.capture
		if capture == nil {
			capture = audio.Capture
		}
		err := capture(ctx, conversationWriter(func(pcm []byte) error {
			// Native capture has no echo cancellation. Drop speaker output and
			// the microphone buffers that can still contain it after playback.
			if !s.headphone && (s.playing.Load() || time.Now().UnixNano() < s.listenAfter.Load()) {
				s.level.Store(0)
				return nil
			}
			s.level.Store(math.Float64bits(audio.Level(pcm)))
			a.requestRender()
			return voice.SendAudio(audio.RealtimePCM(pcm))
		}))
		if err == nil && ctx.Err() == nil {
			err = errors.New("microphone stopped unexpectedly")
		}
		select {
		case failures <- err:
		case <-ctx.Done():
		}
	}()
	var pcm []byte
	var item string
	var index int
	var playCancel context.CancelFunc
	var playStarted time.Time
	var durationMS int
	var playingItem string
	var playingIndex int
	var playbackID uint64
	type playbackResult struct {
		id  uint64
		err error
	}
	playbackDone := make(chan playbackResult, 4)
	type utterance struct {
		item  string
		index int
		pcm   []byte
	}
	var queued []utterance
	queuedBytes := 0
	startPlayback := func(answer utterance) {
		playing, stop := context.WithCancel(ctx)
		playCancel, playStarted = stop, time.Now()
		durationMS = len(answer.pcm) * 1000 / (audio.RealtimeSampleRate * 2)
		playingItem, playingIndex = answer.item, answer.index
		playbackID++
		s.playing.Store(true)
		a.requestRender()
		play := a.voice.play
		if play == nil {
			play = audio.Play
		}
		go func(id uint64) {
			err := play(playing, answer.pcm)
			select {
			case playbackDone <- playbackResult{id: id, err: err}:
			case <-ctx.Done():
			}
		}(playbackID)
	}
	defer func() {
		if playCancel != nil {
			playCancel()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-voice.Done():
			return voice.Err()
		case err := <-failures:
			if err != nil {
				if cause := voice.Err(); cause != nil {
					return cause
				}
				return err
			}
		case result := <-playbackDone:
			if result.id != playbackID {
				continue
			}
			if playCancel != nil {
				playCancel()
				playCancel = nil
			}
			playingItem = ""
			if result.err != nil && !errors.Is(result.err, context.Canceled) {
				return result.err
			}
			if len(queued) > 0 {
				answer := queued[0]
				queued[0] = utterance{}
				queued = queued[1:]
				queuedBytes -= len(answer.pcm)
				startPlayback(answer)
			} else {
				s.listenAfter.Store(time.Now().Add(300 * time.Millisecond).UnixNano())
				s.playing.Store(false)
				a.requestRender()
			}
		case event := <-events:
			switch event.Type {
			case "ready":
				s.ready.Store(true)
				a.requestRender()
			case "error":
				return errors.New(event.Message)
			case "notice":
				a.post(func() {
					if a.conversation == s {
						a.showToast(event.Message, theme.Default.Yellow)
					}
				})
			case "audio":
				chunk, err := base64.StdEncoding.DecodeString(event.Audio)
				if err != nil {
					return err
				}
				if item != event.ItemID {
					pcm = nil
					item, index = event.ItemID, event.ContentIndex
				}
				if len(pcm)+len(chunk) > 16<<20 {
					return errors.New("voice answer is too long for terminal playback")
				}
				pcm = append(pcm, chunk...)
			case "audio_done":
				if len(pcm) == 0 || event.ItemID != item {
					continue
				}
				answer := utterance{item: item, index: index, pcm: pcm}
				if playingItem != "" {
					if len(queued) >= 32 || queuedBytes+len(pcm) > 16<<20 {
						return errors.New("voice playback cannot keep up")
					}
					queued = append(queued, answer)
					queuedBytes += len(pcm)
				} else {
					startPlayback(answer)
				}
				item = ""
				pcm = nil
			case "interrupted":
				if playingItem != "" {
					played := min(durationMS, int(time.Since(playStarted).Milliseconds()))
					if err := voice.Truncate(playingItem, playingIndex, played); err != nil {
						return err
					}
				}
				if item != "" && item != playingItem {
					if err := voice.Truncate(item, index, 0); err != nil {
						return err
					}
				}
				for _, answer := range queued {
					if err := voice.Truncate(answer.item, answer.index, 0); err != nil {
						return err
					}
				}
				queued, queuedBytes = nil, 0
				playingItem, item = "", ""
				playbackID++ // Ignore completion from the cancelled player.
				if playCancel != nil {
					playCancel()
					playCancel = nil
				}
				s.listenAfter.Store(time.Now().Add(300 * time.Millisecond).UnixNano())
				s.playing.Store(false)
				a.requestRender()
				pcm = nil
			}
		}
	}
}

type conversationWriter func([]byte) error

func (w conversationWriter) Write(p []byte) (int, error) {
	if err := w(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
