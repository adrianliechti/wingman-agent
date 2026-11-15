package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/filewatch"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/uuid"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type resourceWatch struct {
	state   types.ResourceWatchState
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
		return nil, ahp.InvalidParams("invalid watch patterns")
	}
	for _, pattern := range patterns.Items {
		if len(pattern) > 2048 || !doublestar.ValidatePattern(pattern) || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "../") {
			return nil, ahp.InvalidParams("invalid relative watch pattern")
		}
	}
	return patterns.Items, nil
}

func (p *Project) createResourceWatch(raw json.RawMessage) (any, error) {
	var params types.CreateResourceWatchParams
	if json.Unmarshal(raw, &params) != nil || params.Channel != types.RootResourceURI {
		return nil, ahp.InvalidParams("root channel required")
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
	w := &resourceWatch{state: types.ResourceWatchState{Root: params.Uri, Recursive: params.Recursive != nil && *params.Recursive, Includes: params.Includes, Excludes: params.Excludes}, touched: time.Now()}
	w.scanner = filewatch.NewScanner(p.workspace.Root.FS(), filepath.ToSlash(rel), filewatch.Options{
		Recursive: w.state.Recursive,
		Skip:      func(rel string, _ fs.DirEntry) bool { return matchesWatch(excludes, rel) },
		Track:     func(rel string, _ fs.DirEntry) bool { return len(includes) == 0 || matchesWatch(includes, rel) },
		Limit:     100000,
	})
	// A watch of something that does not exist would never report it.
	if _, err := p.workspace.Root.Stat(rel); err != nil {
		return nil, err
	}
	p.host.mu.Lock()
	if p.watches == nil {
		p.watches = map[string]*resourceWatch{}
	}
	full := len(p.watches) >= 64
	p.host.mu.Unlock()
	if full {
		return nil, ahp.InvalidParams("resource watch limit reached")
	}
	if _, err := w.scanner.Changes(p.ctx); err != nil {
		if errors.Is(err, filewatch.ErrLimit) {
			return nil, ahp.InvalidParams("resource watch exceeds 100000 entries; narrow its root or exclusions")
		}
		return nil, err
	}
	uri := "ahp-resource-watch:/" + uuid.NewString()
	p.host.mu.Lock()
	if p.host.ctx.Err() != nil || len(p.watches) >= 64 {
		p.host.mu.Unlock()
		return nil, ahp.InvalidParams("resource watch unavailable")
	}
	p.watches[uri] = w
	p.host.mu.Unlock()
	p.watchOnce.Do(func() { p.background.Go(p.runResourceWatches) })
	return types.CreateResourceWatchResult{Channel: uri}, nil
}

func matchesWatch(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matched, _ := doublestar.Match(pattern, name); matched || name == strings.TrimSuffix(pattern, "/") || strings.HasPrefix(name, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
	}
	return false
}

func (p *Project) runResourceWatches() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
		}
		p.host.mu.Lock()
		active := map[string]*resourceWatch{}
		for uri, w := range p.watches {
			subscribed := false
			for c := range p.host.clients {
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
		p.host.mu.Unlock()
		for uri, w := range active {
			scanned, err := w.scanner.Changes(p.ctx)
			if err != nil || len(scanned) == 0 {
				continue // the scanner keeps its baseline across transient read errors
			}
			kinds := map[filewatch.ChangeType]types.ResourceChangeType{filewatch.Created: types.ResourceChangeTypeAdded, filewatch.Changed: types.ResourceChangeTypeUpdated, filewatch.Deleted: types.ResourceChangeTypeDeleted}
			changes := make([]types.ResourceChange, 0, len(scanned))
			for _, change := range scanned {
				changes = append(changes, types.ResourceChange{Uri: p.fileURI(change.Path), Type: kinds[change.Type]})
			}
			slices.SortFunc(changes, func(a, b types.ResourceChange) int { return strings.Compare(a.Uri, b.Uri) })
			p.host.mu.Lock()
			if p.watches[uri] == w {
				data, _ := json.Marshal(map[string]any{"items": changes})
				p.host.publish(uri, types.StateAction{Value: &types.ResourceWatchChangedAction{Type: types.ActionTypeResourceWatchChanged, Changes: data}}, nil, nil)
			}
			p.host.mu.Unlock()
		}
	}
}
