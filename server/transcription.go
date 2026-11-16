package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/adrianliechti/wingman-agent/pkg/audio"
)

type transcriberProvider interface {
	Transcriber(sessionID string) (*audio.Transcriber, bool)
}

// Two minutes of 16 kHz mono PCM16 is under 4 MB.
const maxTranscriptionBytes = 8 << 20

// Voice input is per backend so a draft chat can dictate its first prompt.
func (b *backendRuntime) transcriber() (*audio.Transcriber, bool) {
	provider, ok := b.agent.(transcriberProvider)
	if !ok {
		return nil, false
	}
	return provider.Transcriber("")
}

func handleTranscriptionStatus(b *backendRuntime, w http.ResponseWriter, r *http.Request) {
	_, ok := b.transcriber()
	writeJSON(w, struct {
		Available bool `json:"available"`
	}{ok})
}

func handleTranscribe(b *backendRuntime, w http.ResponseWriter, r *http.Request) {
	tr, ok := b.transcriber()
	if !ok {
		http.Error(w, "voice input is not available", http.StatusNotFound)
		return
	}
	wav, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTranscriptionBytes))
	if err != nil {
		status := http.StatusBadRequest
		if errors.As(err, new(*http.MaxBytesError)) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	text, err := tr.Transcribe(r.Context(), wav)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, struct {
		Text string `json:"text"`
	}{text})
}
