package agent

import (
	"context"
	"io"
	"strings"

	"github.com/openai/openai-go/v3"
)

// ProviderClient exposes the small provider API surface used by optional
// features outside the main agent. It uses the same endpoint and credentials
// as DefaultConfig without creating a second telemetry pipeline.
type ProviderClient struct {
	client *openai.Client
}

func NewProviderClient() *ProviderClient {
	client := createClient()
	return &ProviderClient{client: &client}
}

// AvailableModelIDs returns the model IDs advertised by the configured
// OpenAI-compatible provider.
func (p *ProviderClient) AvailableModelIDs(ctx context.Context) ([]string, error) {
	return availableModelIDs(ctx, p.client)
}

// TranscribeAudio sends one bounded audio recording to the configured
// provider's OpenAI-compatible transcription endpoint.
func (p *ProviderClient) TranscribeAudio(
	ctx context.Context,
	model, filename, contentType string,
	audio io.Reader,
) (string, error) {
	return transcribeAudio(ctx, p.client, model, filename, contentType, audio)
}

// AvailableModelIDs and TranscribeAudio let Config act as the provider for
// optional features while sharing its existing HTTP client.
func (c *Config) AvailableModelIDs(ctx context.Context) ([]string, error) {
	return availableModelIDs(ctx, c.client)
}

func (c *Config) TranscribeAudio(
	ctx context.Context,
	model, filename, contentType string,
	audio io.Reader,
) (string, error) {
	return transcribeAudio(ctx, c.client, model, filename, contentType, audio)
}

func availableModelIDs(ctx context.Context, client *openai.Client) ([]string, error) {
	resp, err := client.Models.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(resp.Data))
	for _, candidate := range resp.Data {
		ids = append(ids, candidate.ID)
	}
	return ids, nil
}

func transcribeAudio(
	ctx context.Context,
	client *openai.Client,
	model, filename, contentType string,
	audio io.Reader,
) (string, error) {
	resp, err := client.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		File:  openai.File(audio, filename, contentType),
		Model: openai.AudioModel(model),
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}
