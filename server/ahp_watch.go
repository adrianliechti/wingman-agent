package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/uuid"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type ahpResourceWatch struct {
	state              t.ResourceWatchState
	rel                string
	includes, excludes []string
	files              map[string]watchStamp // owned by the watch loop after initial registration
	touched            time.Time             // host.mu
}
type watchStamp struct {
	size     int64
	modified int64
	mode     fs.FileMode
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

func (h *ahpHost) createResourceWatch(raw json.RawMessage) (any, error) {
	var p t.CreateResourceWatchParams
	if json.Unmarshal(raw, &p) != nil || p.Channel != t.RootResourceURI {
		return nil, ahpInvalid("root channel required")
	}
	rel, err := h.resourcePath(p.Uri)
	if err != nil {
		return nil, err
	}
	includes, err := watchPatterns(p.Includes)
	if err != nil {
		return nil, err
	}
	excludes, err := watchPatterns(p.Excludes)
	if err != nil {
		return nil, err
	}
	w := &ahpResourceWatch{state: t.ResourceWatchState{Root: p.Uri, Recursive: p.Recursive != nil && *p.Recursive, Includes: p.Includes, Excludes: p.Excludes}, rel: filepath.ToSlash(rel), includes: includes, excludes: excludes, touched: time.Now()}
	h.mu.Lock()
	if h.watches == nil {
		h.watches = map[string]*ahpResourceWatch{}
	}
	for uri, existing := range h.watches {
		if reflect.DeepEqual(existing.state, w.state) {
			existing.touched = time.Now()
			h.mu.Unlock()
			return t.CreateResourceWatchResult{Channel: uri}, nil
		}
	}
	full := len(h.watches) >= 64
	h.mu.Unlock()
	if full {
		return nil, ahpInvalid("resource watch limit reached")
	}
	w.files, err = h.scanWatch(w)
	if err != nil {
		return nil, err
	}
	uri := "ahp-resource-watch:/" + uuid.NewString()
	h.mu.Lock()
	if h.closed || len(h.watches) >= 64 {
		h.mu.Unlock()
		return nil, ahpInvalid("resource watch unavailable")
	}
	h.watches[uri] = w
	h.mu.Unlock()
	h.watchOnce.Do(func() { h.server.background.Go(h.runResourceWatches) })
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

func (h *ahpHost) scanWatch(w *ahpResourceWatch) (map[string]watchStamp, error) {
	files := map[string]watchStamp{}
	visited := 0
	err := fs.WalkDir(h.server.workspace.Root.FS(), w.rel, func(name string, entry fs.DirEntry, err error) error {
		select {
		case <-h.done:
			return fs.SkipAll
		default:
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(filepath.FromSlash(w.rel), filepath.FromSlash(name))
		rel = filepath.ToSlash(rel)
		if matchesWatch(w.excludes, rel) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		visited++
		if visited > 100000 {
			return ahpInvalid("resource watch exceeds 100000 entries; narrow its root or exclusions")
		}
		if len(w.includes) == 0 || matchesWatch(w.includes, rel) {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			files[name] = watchStamp{size: info.Size(), modified: info.ModTime().UnixNano(), mode: info.Mode()}
		}
		if !w.state.Recursive && name != w.rel && entry.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	return files, err
}

func (h *ahpHost) runResourceWatches() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
		}
		h.mu.Lock()
		active := map[string]*ahpResourceWatch{}
		for uri, w := range h.watches {
			subscribed := false
			for c := range h.clients {
				if c.subscriptions[uri] {
					subscribed = true
					break
				}
			}
			if subscribed {
				w.touched = time.Now()
				active[uri] = w
			} else if time.Since(w.touched) > time.Minute {
				delete(h.watches, uri)
			}
		}
		h.mu.Unlock()
		for uri, w := range active {
			files, err := h.scanWatch(w)
			if err != nil {
				continue
			} // retain the baseline across transient read errors
			changes := []t.ResourceChange{}
			for name, stamp := range files {
				old, exists := w.files[name]
				if exists && old == stamp {
					continue
				}
				kind := t.ResourceChangeTypeUpdated
				if !exists {
					kind = t.ResourceChangeTypeAdded
				}
				changes = append(changes, t.ResourceChange{Uri: h.fileURI(name), Type: kind})
			}
			for name := range w.files {
				if _, exists := files[name]; !exists {
					changes = append(changes, t.ResourceChange{Uri: h.fileURI(name), Type: t.ResourceChangeTypeDeleted})
				}
			}
			w.files = files
			if len(changes) == 0 {
				continue
			}
			slices.SortFunc(changes, func(a, b t.ResourceChange) int { return strings.Compare(a.Uri, b.Uri) })
			h.mu.Lock()
			if h.watches[uri] == w {
				data, _ := json.Marshal(map[string]any{"items": changes})
				h.publish(uri, t.StateAction{Value: &t.ResourceWatchChangedAction{Type: t.ActionTypeResourceWatchChanged, Changes: data}}, nil, nil)
			}
			h.mu.Unlock()
		}
	}
}

func (h *ahpHost) fileURI(relative string) string {
	base, _ := url.Parse(h.directoryURI())
	base.Path = path.Join(base.Path, filepath.ToSlash(relative))
	return base.String()
}
