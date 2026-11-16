package agent

import "testing"

func TestTranscriptionModel(t *testing.T) {
	gateway := []string{"gpt-6-sol", "claude-opus-5-5", "gemini-3.8-flash", "mistral-large-4",
		"gpt-transcribe", "gpt-live-transcribe", "gemini-3.5-transcribe", "gemini-3.5-transcribe-live",
		"mai-transcribe-2", "voxtral-mini-transcribe-3", "gpt-4o-mini-tts"}

	for _, c := range []struct {
		name     string
		models   []string
		selected string
		options  Options
		want     string
	}{
		{name: "same family as chat model", models: gateway, selected: "gpt-6-sol", want: "gpt-transcribe"},
		{name: "follows session model family", models: gateway, selected: "gemini-3.8-flash", want: "gemini-3.5-transcribe"},
		{name: "sorted fallback without family match", models: gateway, selected: "claude-opus-5-5", want: "gemini-3.5-transcribe"},
		{name: "live models do not count", models: []string{"claude-opus-5-5", "gpt-live-transcribe", "gpt-realtime-2.1"}, want: ""},
		{name: "no transcription model", models: []string{"claude-opus-5-5", "gpt-6-sol"}, want: ""},
		{name: "before discovery", models: nil, want: ""},
		{name: "pinned model", models: gateway, options: Options{TranscribeModel: "whisper-large"}, want: "whisper-large"},
		{name: "pinned without discovery", models: nil, options: Options{TranscribeModel: "whisper-large"}, want: "whisper-large"},
		{name: "disabled", models: gateway, options: Options{DisableVoice: true}, want: ""},
		{name: "disabled wins over pin", models: gateway, options: Options{DisableVoice: true, TranscribeModel: "gpt-transcribe"}, want: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := upstreamAgent(c.models...)
			if c.models == nil {
				a.upstreamModels = nil
			}
			a.cfg.Options = c.options
			s := &sessionState{modelID: c.selected}
			got, ok := a.transcriptionModel(s)
			if got != c.want || ok != (c.want != "") {
				t.Fatalf("transcriptionModel = %q, %v; want %q, %v", got, ok, c.want, c.want != "")
			}
		})
	}
}

func TestRealtimeModel(t *testing.T) {
	for _, c := range []struct {
		name    string
		models  []string
		options Options
		want    string
	}{
		{name: "discovered", models: []string{"gpt-6-sol", "gpt-realtime-2.1", "gpt-4o-mini-realtime-preview", "gpt-transcribe"}, want: "gpt-realtime-2.1"},
		{name: "no compatible protocol", models: []string{"gpt-live-1", "gpt-live-transcribe"}},
		{name: "pinned", options: Options{RealtimeModel: "voice-deployment"}, want: "voice-deployment"},
		{name: "disabled", options: Options{RealtimeModel: "voice-deployment", DisableVoice: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := DefaultConfig()
			if err != nil {
				t.Fatal(err)
			}
			a := upstreamAgent(c.models...)
			cfg.Options = c.options
			a.cfg = cfg
			model, ok := a.Realtime("")
			if ok != (c.want != "") || ok && model.Model != c.want {
				t.Fatalf("realtime model = %+v, %v; want %q", model, ok, c.want)
			}
		})
	}
}
