package agent

import (
	"slices"

	"github.com/adrianliechti/wingman-agent/pkg/audio"
	"github.com/adrianliechti/wingman-agent/pkg/model"
)

// Transcriber returns the speech-to-text client for voice input in a session.
// ok is false when voice is disabled or the backend offers no transcription model.
func (a *Agent) Transcriber(sessionID string) (*audio.Transcriber, bool) {
	id, ok := a.transcriptionModel(a.session(sessionID))
	if !ok {
		return nil, false
	}
	return a.cfg.Transcriber(id), true
}

// Realtime resolves the voice model independently of the session's coding model.
func (a *Agent) Realtime(sessionID string) (*audio.Realtime, bool) {
	if a.cfg.DisableVoice {
		return nil, false
	}
	id := a.cfg.RealtimeModel
	if id == "" {
		a.modelMu.Lock()
		var candidates []string
		for name := range a.upstreamModels {
			if audio.IsRealtimeModel(name) {
				candidates = append(candidates, name)
			}
		}
		a.modelMu.Unlock()
		slices.Sort(candidates)
		if len(candidates) == 0 {
			return nil, false
		}
		id = candidates[0]
	}
	return a.cfg.Realtime(id), true
}

// transcriptionModel prefers the session model's family, then sorted order,
// so the choice stays stable across discovery refreshes.
func (a *Agent) transcriptionModel(s *sessionState) (string, bool) {
	if a.cfg.DisableVoice {
		return "", false
	}
	// Explicit deployments need not appear in model discovery.
	if a.cfg.TranscribeModel != "" {
		return a.cfg.TranscribeModel, true
	}

	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	var candidates []string
	for id := range a.upstreamModels {
		if audio.IsTranscriptionModel(id) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	slices.Sort(candidates)
	family := model.Family(a.selectedModelLocked(s, model.Available(a.upstreamModels)))
	if i := slices.IndexFunc(candidates, func(id string) bool { return model.Family(id) == family }); i >= 0 {
		return candidates[i], true
	}
	return candidates[0], true
}
