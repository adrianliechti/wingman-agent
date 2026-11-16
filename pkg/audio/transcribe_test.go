package audio

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestTranscribe(t *testing.T) {
	wav := WAV([]byte{1, 0, 2, 0})

	for _, c := range []struct {
		name   string
		tr     Transcriber
		fields map[string]string
	}{
		{"options", Transcriber{Model: "gpt-transcribe", Language: "de", Prompt: "wingman-agent main"}, map[string]string{"model": "gpt-transcribe", "language": "de", "prompt": "wingman-agent main"}},
		{"defaults", Transcriber{Model: "voxtral-mini-transcribe-3"}, map[string]string{"model": "voxtral-mini-transcribe-3"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Fatalf("parse multipart: %v", err)
				}
				for key := range r.MultipartForm.Value {
					if _, ok := c.fields[key]; !ok {
						t.Errorf("unexpected field %q = %q", key, r.FormValue(key))
					}
				}
				for key, want := range c.fields {
					if got := r.FormValue(key); got != want {
						t.Errorf("field %q = %q, want %q", key, got, want)
					}
				}
				file, header, err := r.FormFile("file")
				if err != nil {
					t.Fatalf("file part: %v", err)
				}
				defer file.Close()
				if header.Filename != "audio.wav" || header.Header.Get("Content-Type") != "audio/wav" {
					t.Errorf("file = %q (%s), want audio.wav (audio/wav)", header.Filename, header.Header.Get("Content-Type"))
				}
				if body, _ := io.ReadAll(file); !bytes.Equal(body, wav) {
					t.Errorf("file body = %x, want %x", body, wav)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"text":"  hello world\n"}`)
			}))
			defer srv.Close()

			tr := c.tr
			tr.Client = openai.NewClient(option.WithBaseURL(srv.URL+"/v1"), option.WithAPIKey("test"), option.WithMaxRetries(0))
			text, err := tr.Transcribe(context.Background(), wav)
			if err != nil {
				t.Fatal(err)
			}
			if text != "hello world" {
				t.Errorf("text = %q, want %q", text, "hello world")
			}
		})
	}
}

func TestTranscribeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"model not found"}}`, http.StatusNotFound)
	}))
	defer srv.Close()

	tr := Transcriber{Client: openai.NewClient(option.WithBaseURL(srv.URL+"/v1"), option.WithAPIKey("test"), option.WithMaxRetries(0)), Model: "missing"}
	if _, err := tr.Transcribe(context.Background(), WAV(nil)); err == nil {
		t.Fatal("Transcribe succeeded, want error")
	}
}

func TestIsTranscriptionModel(t *testing.T) {
	for id, want := range map[string]bool{
		"gpt-transcribe":             true,
		"gpt-4o-mini-transcribe":     true,
		"gemini-3.5-transcribe":      true,
		"mai-transcribe-2":           true,
		"voxtral-mini-transcribe-3":  true,
		"whisper-1":                  true,
		"Whisper-Large-V3":           true,
		"gpt-live-transcribe":        false,
		"gemini-3.5-transcribe-live": false,
		"gpt-realtime-2.1":           false,
		"gpt-4o-mini-tts":            false,
		"voxtral-mini-tts-3":         false,
		"gpt-6-sol":                  false,
	} {
		if got := IsTranscriptionModel(id); got != want {
			t.Errorf("IsTranscriptionModel(%q) = %v, want %v", id, got, want)
		}
	}
}
