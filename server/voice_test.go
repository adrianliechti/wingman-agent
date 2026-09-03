package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/voice"
)

type voiceBackend struct {
	model string
	audio []byte
}

func (b *voiceBackend) AvailableModelIDs(context.Context) ([]string, error) {
	return []string{"gpt-5.6-sol", "gpt-4o-transcribe"}, nil
}

func (b *voiceBackend) TranscribeAudio(_ context.Context, model, _, _ string, audio io.Reader) (string, error) {
	b.model = model
	b.audio, _ = io.ReadAll(audio)
	return "hello from voice", nil
}

func TestHandleVoiceTranscription(t *testing.T) {
	backend := new(voiceBackend)
	service := voice.New(backend)
	if _, err := service.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &Server{voice: service}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("audio", "voice.webm")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("fake audio"))
	_ = form.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/voice/transcriptions", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	res := httptest.NewRecorder()
	s.handleVoiceTranscription(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var got struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "hello from voice" || got.Model != "gpt-4o-transcribe" {
		t.Fatalf("response = %+v", got)
	}
	if backend.model != got.Model || string(backend.audio) != "fake audio" {
		t.Fatalf("backend model = %q, audio = %q", backend.model, backend.audio)
	}
}

func TestHandleVoiceTranscriptionUnavailable(t *testing.T) {
	s := &Server{voice: voice.New(&voiceBackend{})}
	req := httptest.NewRequest(http.MethodPost, "/api/voice/transcriptions", nil)
	res := httptest.NewRecorder()
	s.handleVoiceTranscription(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}
