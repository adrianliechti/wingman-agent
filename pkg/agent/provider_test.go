package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestProviderClientListsModelsAndTranscribesAudio(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-transcribe","object":"model","created":0,"owned_by":"openai"}]}`)
		case "/v1/audio/transcriptions":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse multipart: %v", err)
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			if got := r.FormValue("model"); got != "gpt-transcribe" {
				t.Errorf("model = %q", got)
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Errorf("audio file: %v", err)
				http.Error(w, "missing audio", http.StatusBadRequest)
				return
			}
			defer file.Close()
			data, _ := io.ReadAll(file)
			if header.Filename != "voice.wav" || string(data) != "audio bytes" {
				t.Errorf("file = %q, contents = %q", header.Filename, data)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"hello from audio"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := openai.NewClient(
		option.WithBaseURL(server.URL+"/v1"),
		option.WithAPIKey("test"),
	)
	provider := &ProviderClient{client: &client}
	models, err := provider.AvailableModelIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "gpt-transcribe" {
		t.Fatalf("models = %v", models)
	}
	text, err := provider.TranscribeAudio(
		context.Background(),
		"gpt-transcribe",
		"voice.wav",
		"audio/wav",
		strings.NewReader("audio bytes"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello from audio" {
		t.Fatalf("text = %q", text)
	}
}
