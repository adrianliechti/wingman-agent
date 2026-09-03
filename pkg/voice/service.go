package voice

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("voice transcription is unavailable")

// modelPriority is ordered from highest to lowest transcription quality.
// /v1/models does not expose modality metadata, so discovery deliberately
// intersects the advertised IDs with these known transcription families.
var modelPriority = []string{
	"gpt-transcribe",
	"gpt-4o-transcribe",
	"gpt-4o-mini-transcribe",
	"whisper-1",
}

type Backend interface {
	AvailableModelIDs(context.Context) ([]string, error)
	TranscribeAudio(context.Context, string, string, string, io.Reader) (string, error)
}

type Capability struct {
	Model string `json:"model"`
}

type Service struct {
	backend Backend

	discoverMu sync.Mutex
	mu         sync.RWMutex
	model      string

	requests chan struct{}
}

func New(backend Backend) *Service {
	return &Service{backend: backend, requests: make(chan struct{}, 1)}
}

// Discover refreshes voice availability from the configured provider.
func (s *Service) Discover(ctx context.Context) (Capability, error) {
	s.discoverMu.Lock()
	defer s.discoverMu.Unlock()

	ids, err := s.backend.AvailableModelIDs(ctx)
	if err != nil {
		s.setModel("")
		return Capability{}, err
	}
	model, _ := SelectModel(ids)
	s.setModel(model)
	return s.Capability(), nil
}

func (s *Service) setModel(model string) {
	s.mu.Lock()
	s.model = model
	s.mu.Unlock()
}

func (s *Service) Capability() Capability {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Capability{Model: s.model}
}

func (s *Service) Available() bool {
	return s.Capability().Model != ""
}

func (s *Service) Transcribe(
	ctx context.Context,
	filename, contentType string,
	audio io.Reader,
) (string, error) {
	model := s.Capability().Model
	if model == "" {
		return "", ErrUnavailable
	}

	select {
	case s.requests <- struct{}{}:
		defer func() { <-s.requests }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	text, err := s.backend.TranscribeAudio(ctx, model, filename, contentType, audio)
	return strings.TrimSpace(text), err
}

// SelectModel chooses the best compatible advertised ID. Canonical aliases
// win over snapshots; when only snapshots are present, the newest one wins.
func SelectModel(ids []string) (string, bool) {
	for _, family := range modelPriority {
		for _, id := range ids {
			if modelLeaf(id) == family {
				return id, true
			}
		}

		var snapshots []string
		for _, id := range ids {
			leaf := modelLeaf(id)
			if !strings.HasPrefix(leaf, family+"-") {
				continue
			}
			date := strings.TrimPrefix(leaf, family+"-")
			if _, err := time.Parse("2006-01-02", date); err == nil {
				snapshots = append(snapshots, id)
			}
		}
		if len(snapshots) > 0 {
			slices.SortFunc(snapshots, func(a, b string) int {
				return strings.Compare(modelLeaf(b), modelLeaf(a))
			})
			return snapshots[0], true
		}
	}
	return "", false
}

func modelLeaf(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if slash := strings.LastIndexByte(id, '/'); slash >= 0 {
		return id[slash+1:]
	}
	return id
}
