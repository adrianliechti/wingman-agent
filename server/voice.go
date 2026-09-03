package server

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/voice"
)

const maxVoiceUploadBytes = 25 << 20

var voiceAudioExtensions = map[string]bool{
	".flac": true,
	".m4a":  true,
	".mp3":  true,
	".mp4":  true,
	".mpeg": true,
	".mpga": true,
	".ogg":  true,
	".wav":  true,
	".webm": true,
}

func (s *Server) handleVoiceTranscription(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.voice == nil || !s.voice.Available() {
		http.Error(w, voice.ErrUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}

	// Leave room for multipart framing while preserving the upstream audio
	// endpoint's 25 MiB file limit.
	r.Body = http.MaxBytesReader(w, r.Body, maxVoiceUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "audio recording is too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid audio upload", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, header, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "missing audio recording", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if header.Size <= 0 {
		http.Error(w, "audio recording is empty", http.StatusBadRequest)
		return
	}
	if header.Size > maxVoiceUploadBytes {
		http.Error(w, "audio recording is too large", http.StatusRequestEntityTooLarge)
		return
	}

	filename := filepath.Base(header.Filename)
	if filename == "." || filename == "" {
		filename = "voice.webm"
	}
	if !voiceAudioExtensions[strings.ToLower(filepath.Ext(filename))] {
		http.Error(w, "unsupported audio format", http.StatusBadRequest)
		return
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	text, err := s.voice.Transcribe(r.Context(), filename, contentType, file)
	if err != nil {
		if errors.Is(err, voice.ErrUnavailable) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, fmt.Sprintf("voice transcription failed: %v", err), http.StatusBadGateway)
		return
	}

	writeJSON(w, struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}{Text: text, Model: s.voice.Capability().Model})
}
