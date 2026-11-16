package audio

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/internal/provider"
)

// WINGMAN_URL=http://localhost:4242 WINGMAN_LIVE_TRANSCRIBE_MODELS=gpt-transcribe,voxtral-mini-transcribe-3
// go test ./pkg/audio -run TestLiveTranscribe -count=1 -v
//
// Speech comes from WINGMAN_LIVE_TTS_MODEL (default gpt-4o-mini-tts) on the same endpoint.
func TestLiveTranscribe(t *testing.T) {
	models := strings.FieldsFunc(os.Getenv("WINGMAN_LIVE_TRANSCRIBE_MODELS"), func(r rune) bool { return r == ',' })
	if len(models) == 0 {
		t.Skip("set WINGMAN_LIVE_TRANSCRIBE_MODELS to run real transcription")
	}
	tts := os.Getenv("WINGMAN_LIVE_TTS_MODEL")
	if tts == "" {
		tts = "gpt-4o-mini-tts"
	}

	baseURL, token := provider.Wingman("", "")
	client := openai.NewClient(option.WithBaseURL(baseURL+"/v1"), option.WithAPIKey(token))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := client.Audio.Speech.New(ctx, openai.AudioSpeechNewParams{
		Model:          openai.SpeechModel(tts),
		Input:          "Refactor the authentication middleware and run the tests.",
		Voice:          openai.AudioSpeechNewParamsVoiceUnion{OfString: openai.String("alloy")},
		ResponseFormat: openai.AudioSpeechNewParamsResponseFormatWAV,
	})
	if err != nil {
		t.Fatalf("speech: %v", err)
	}
	wav, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatalf("speech body: %v", err)
	}

	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			tr := Transcriber{Client: client, Model: model, Language: "en", Prompt: "wingman-agent"}
			text, err := tr.Transcribe(ctx, wav)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%q", text)
			lower := strings.ToLower(text)
			for _, word := range []string{"authentication", "middleware", "tests"} {
				if !strings.Contains(lower, word) {
					t.Errorf("transcript %q is missing %q", text, word)
				}
			}
		})
	}
}
