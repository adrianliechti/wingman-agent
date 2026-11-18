package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/audio"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type realtimeProvider interface {
	Realtime(sessionID string) (*audio.Realtime, bool)
}

func (b *backendRuntime) realtime(sid string) (*audio.Realtime, bool) {
	provider, ok := b.agent.(realtimeProvider)
	if !ok {
		return nil, false
	}
	return provider.Realtime(sid)
}

func handleConversationStatus(b *backendRuntime, w http.ResponseWriter, r *http.Request) {
	_, ok := b.realtime("")
	writeJSON(w, struct {
		Available bool `json:"available"`
	}{ok})
}

func handleConversation(c *sessionController, w http.ResponseWriter, r *http.Request) {
	if !ahp.OriginAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	model, ok := c.backend.realtime(c.ref.SessionID)
	if !ok {
		http.Error(w, "voice conversation is not available", http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(128 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	defer stop()
	write := func(event code.ConversationEvent) error {
		writing, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return wsjson.Write(writing, conn, event)
	}
	// Read immediately, including during upstream setup, so a closed browser
	// cancels the opening handshake and cannot leave an orphaned voice session.
	type input struct {
		Type         string `json:"type"`
		Audio        string `json:"audio"`
		ItemID       string `json:"item_id"`
		ContentIndex int    `json:"content_index"`
		PlayedMS     int    `json:"played_ms"`
	}
	inputs := make(chan input, 16)
	go func() {
		defer cancel()
		for {
			var event input
			if err := wsjson.Read(ctx, conn, &event); err != nil {
				return
			}
			select {
			case inputs <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	voice, err := code.StartConversation(ctx, model, c.backend.agent, c.backend.turns, c.ref.SessionID, write)
	if err != nil {
		_ = write(code.ConversationEvent{Type: "error", Message: err.Error()})
		return
	}
	defer voice.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-voice.Done():
			return
		case event := <-inputs:
			switch event.Type {
			case "audio":
				var pcm []byte
				pcm, err = base64.StdEncoding.DecodeString(event.Audio)
				if err == nil {
					err = voice.SendAudio(pcm)
				}
			case "truncate":
				err = voice.Truncate(event.ItemID, event.ContentIndex, event.PlayedMS)
			default:
				err = nil
			}
			if err != nil {
				_ = write(code.ConversationEvent{Type: "error", Message: err.Error()})
				return
			}
		}
	}
}
