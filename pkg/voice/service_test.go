package voice

import (
	"context"
	"io"
	"strings"
	"testing"
)

type fakeBackend struct {
	models []string
	model  string
	audio  string
}

func (f *fakeBackend) AvailableModelIDs(context.Context) ([]string, error) {
	return f.models, nil
}

func (f *fakeBackend) TranscribeAudio(_ context.Context, model, _, _ string, audio io.Reader) (string, error) {
	f.model = model
	data, _ := io.ReadAll(audio)
	f.audio = string(data)
	return " transcribed prompt ", nil
}

func TestSelectModelPriorityAndSnapshots(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want string
	}{
		{"quality order", []string{"whisper-1", "gpt-4o-mini-transcribe", "gpt-transcribe"}, "gpt-transcribe"},
		{"alias before snapshot", []string{"gpt-4o-transcribe-2026-01-02", "gpt-4o-transcribe"}, "gpt-4o-transcribe"},
		{"newest snapshot", []string{"gpt-4o-mini-transcribe-2025-12-15", "gpt-4o-mini-transcribe-2026-02-01"}, "gpt-4o-mini-transcribe-2026-02-01"},
		{"provider namespace", []string{"openai/gpt-4o-transcribe"}, "openai/gpt-4o-transcribe"},
		{"exclude related models", []string{"gpt-4o-transcribe-diarize", "gpt-live-transcribe"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := SelectModel(tt.ids)
			if got != tt.want {
				t.Fatalf("SelectModel(%v) = %q, want %q", tt.ids, got, tt.want)
			}
		})
	}
}

func TestServiceDiscoversAndTranscribes(t *testing.T) {
	backend := &fakeBackend{models: []string{"unrelated", "gpt-4o-transcribe"}}
	service := New(backend)
	capability, err := service.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability.Model != "gpt-4o-transcribe" {
		t.Fatalf("model = %q", capability.Model)
	}

	text, err := service.Transcribe(context.Background(), "voice.wav", "audio/wav", strings.NewReader("audio"))
	if err != nil {
		t.Fatal(err)
	}
	if text != "transcribed prompt" || backend.model != capability.Model || backend.audio != "audio" {
		t.Fatalf("transcription = %q, model = %q, audio = %q", text, backend.model, backend.audio)
	}
}

func TestServiceUnavailableWithoutCompatibleModel(t *testing.T) {
	service := New(&fakeBackend{models: []string{"gpt-5.6-sol"}})
	if _, err := service.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if service.Available() {
		t.Fatal("service unexpectedly available")
	}
	if _, err := service.Transcribe(context.Background(), "voice.wav", "audio/wav", strings.NewReader("audio")); err != ErrUnavailable {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}
