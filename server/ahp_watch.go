package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/filewatch"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/uuid"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type ahpResourceWatch struct {
	state   t.ResourceWatchState
	scanner *filewatch.Scanner
	touched time.Time // host.mu
}

func watchPatterns(raw *json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var patterns struct {
		Items []string `json:"items"`
	}
	if json.Unmarshal(*raw, &patterns) != nil || len(patterns.Items) > 128 {
		return nil, ahpInvalid("invalid watch patterns")
	}
	for _, pattern := range patterns.Items {
		if len(pattern) > 2048 || !doublestar.ValidatePattern(pattern) || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "../") {
			return nil, ahpInvalid("invalid relative watch pattern")
		}
	}
	return patterns.Items, nil
}

func (p *ahpProject) createResourceWatch(raw json.RawMessage) (any, error) {
	var params t.CreateResourceWatchParams
	if json.Unmarshal(raw, &params) != nil || params.Channel != t.RootResourceURI {
		return nil, ahpInvalid("root channel required")
	}
	rel, err := p.resourcePath(params.Uri)
	if err != nil {
		return nil, err
	}
	includes, err := watchPatterns(params.Includes)
	if err != nil {
		return nil, err
	}
	excludes, err := watchPatterns(params.Excludes)
	if err != nil {
		return nil, err
	}
	w := &ahpResourceWatch{state: t.ResourceWatchState{Root: params.Uri, Recursive: params.Recursive != nil && *params.Recursive, Includes: params.Includes, Excludes: params.Excludes}, touched: time.Now()}
	w.scanner = filewatch.NewScanner(p.server.workspace.Root.FS(), filepath.ToSlash(rel), filewatch.Options{
		Recursive: w.state.Recursive,
		Skip:      func(rel string, _ fs.DirEntry) bool { return matchesWatch(excludes, rel) },
		Track:     func(rel string, _ fs.DirEntry) bool { return len(includes) == 0 || matchesWatch(includes, rel) },
		Limit:     100000,
	})
	p.mu.Lock()
	if p.watches == nil {
		p.watches = map[string]*ahpResourceWatch{}
	}
	for uri, existing := range p.watches {
		if reflect.DeepEqual(existing.state, w.state) {
			existing.touched = time.Now()
			p.mu.Unlock()
			return t.CreateResourceWatchResult{Channel: uri}, nil
		}
	}
	full := len(p.watches) >= 64
	p.mu.Unlock()
	if full {
		return nil, ahpInvalid("resource watch limit reached")
	}
	if _, err := w.scanner.Changes(p.server.ctx); err != nil {
		if errors.Is(err, filewatch.ErrLimit) {
			return nil, ahpInvalid("resource watch exceeds 100000 entries; narrow its root or exclusions")
		}
		return nil, err
	}
	uri := "ahp-resource-watch:/" + uuid.NewString()
	p.mu.Lock()
	if p.closed || len(p.watches) >= 64 {
		p.mu.Unlock()
		return nil, ahpInvalid("resource watch unavailable")
	}
	p.watches[uri] = w
	p.mu.Unlock()
	p.watchOnce.Do(func() { p.server.background.Go(p.runResourceWatches) })
	return t.CreateResourceWatchResult{Channel: uri}, nil
}

func matchesWatch(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matched, _ := doublestar.Match(pattern, name); matched || name == strings.TrimSuffix(pattern, "/") || strings.HasPrefix(name, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
	}
	return false
}

func (p *ahpProject) runResourceWatches() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
		}
		p.mu.Lock()
		active := map[string]*ahpResourceWatch{}
		for uri, w := range p.watches {
			subscribed := false
			for c := range p.clients {
				if c.subscriptions[uri] {
					subscribed = true
					break
				}
			}
			if subscribed {
				w.touched = time.Now()
				active[uri] = w
			} else if time.Since(w.touched) > time.Minute {
				delete(p.watches, uri)
			}
		}
		p.mu.Unlock()
		for uri, w := range active {
			scanned, err := w.scanner.Changes(p.server.ctx)
			if err != nil || len(scanned) == 0 {
				continue // the scanner keeps its baseline across transient read errors
			}
			kinds := map[filewatch.ChangeType]t.ResourceChangeType{filewatch.Created: t.ResourceChangeTypeAdded, filewatch.Changed: t.ResourceChangeTypeUpdated, filewatch.Deleted: t.ResourceChangeTypeDeleted}
			changes := make([]t.ResourceChange, 0, len(scanned))
			for _, change := range scanned {
				changes = append(changes, t.ResourceChange{Uri: p.fileURI(change.Path), Type: kinds[change.Type]})
			}
			slices.SortFunc(changes, func(a, b t.ResourceChange) int { return strings.Compare(a.Uri, b.Uri) })
			p.mu.Lock()
			if p.watches[uri] == w {
				data, _ := json.Marshal(map[string]any{"items": changes})
				p.publish(uri, t.StateAction{Value: &t.ResourceWatchChangedAction{Type: t.ActionTypeResourceWatchChanged, Changes: data}}, nil, nil)
			}
			p.mu.Unlock()
		}
	}
}
