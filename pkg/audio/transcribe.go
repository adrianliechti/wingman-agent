package audio

import (
	"bytes"
	"context"
	"strings"

	"github.com/openai/openai-go/v3"
)

// Transcriber turns recorded speech into text through an OpenAI-compatible
// /audio/transcriptions endpoint.
type Transcriber struct {
	Client openai.Client
	Model  string

	// Language is an ISO-639-1 code; empty lets the model detect it.
	Language string

	// Prompt biases recognition, for example toward project and branch names.
	Prompt string
}

func (t *Transcriber) Transcribe(ctx context.Context, wav []byte) (string, error) {
	params := openai.AudioTranscriptionNewParams{
		File:  openai.File(bytes.NewReader(wav), "audio.wav", "audio/wav"),
		Model: openai.AudioModel(t.Model),
	}
	if t.Language != "" {
		params.Language = openai.String(t.Language)
	}
	if t.Prompt != "" {
		params.Prompt = openai.String(t.Prompt)
	}
	result, err := t.Client.Audio.Transcriptions.New(ctx, params)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Text), nil
}

// IsTranscriptionModel reports whether id names a file transcription model.
// Live and realtime variants only accept streaming sessions.
func IsTranscriptionModel(id string) bool {
	id = strings.ToLower(id)
	if strings.Contains(id, "live") || strings.Contains(id, "realtime") {
		return false
	}
	return strings.Contains(id, "transcribe") || strings.HasPrefix(id, "whisper")
}
