package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type fileSearchHit struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (h *ahpHost) fileExtension(req ahpRequest) (any, error) {
	var p struct {
		Channel string `json:"channel"`
		URI     string `json:"uri"`
		Query   string `json:"query"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.Channel != t.RootResourceURI {
		return nil, ahpInvalid("root channel required")
	}
	if req.Method == "x-wingman/revealResource" {
		rel, err := h.resourcePath(p.URI)
		if err != nil {
			return nil, err
		}
		info, err := h.server.workspace.Root.Stat(rel)
		if err != nil {
			return nil, ahpResourceError(err)
		}
		return struct{}{}, revealPath(filepath.Join(h.server.workspace.RootPath, rel), info.IsDir())
	}
	hits := []fileSearchHit{}
	query := strings.ToLower(strings.TrimSpace(p.Query))
	ctx, cancel := context.WithTimeout(h.server.ctx, 5*time.Second)
	defer cancel()
	visited := 0
	err := fs.WalkDir(h.server.workspace.Root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > 100000 {
			return fs.SkipAll
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(name, ".") || excludedDirs[name]) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || query != "" && !strings.Contains(strings.ToLower(path), query) {
			return nil
		}
		hits = append(hits, fileSearchHit{Path: path, Name: name})
		if len(hits) >= 50 {
			return fs.SkipAll
		}
		return nil
	})
	return hits, err
}

// Searches run independently of the connection's ordered command loop so typing,
// cancellation, and chat actions do not wait for a filesystem traversal.
func (c *ahpConnection) startFileSearch(req ahpRequest) {
	var p struct {
		Channel  string `json:"channel"`
		SearchID string `json:"searchId"`
		workspaceSearchRequest
	}
	if !c.initialized || len(req.Params) > 64<<10 || json.Unmarshal(req.Params, &p) != nil || p.Channel != t.RootResourceURI || p.SearchID == "" {
		c.reply(req.ID, nil, ahpInvalid("initialized root channel and searchId required"))
		return
	}
	if _, err := newWorkspaceSearchConfig(p.workspaceSearchRequest); err != nil {
		c.reply(req.ID, nil, ahpInvalid(err.Error()))
		return
	}
	c.searchMu.Lock()
	if len(c.searches) >= 4 || c.searches[p.SearchID] != nil {
		c.searchMu.Unlock()
		c.reply(req.ID, nil, ahpInvalid("too many searches or duplicate searchId"))
		return
	}
	ctx, cancel := context.WithTimeout(c.resourceCtx, time.Minute)
	c.searches[p.SearchID] = cancel
	c.searchMu.Unlock()
	c.host.server.background.Go(func() {
		defer cancel()
		defer func() { c.searchMu.Lock(); delete(c.searches, p.SearchID); c.searchMu.Unlock() }()
		var summary workspaceSearchEvent
		err := c.host.server.searchWorkspace(ctx, p.workspaceSearchRequest, func(event workspaceSearchEvent) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if event.Type == "done" {
				summary = event
				return nil
			}
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "x-wingman/searchProgress", "params": map[string]any{"searchId": p.SearchID, "file": event.File}})
			if !c.writer.enqueue(data) {
				return errors.New("search receiver disconnected or too slow")
			}
			return nil
		})
		c.reply(req.ID, map[string]any{"files": summary.Files, "matches": summary.Matches, "truncated": summary.Truncated}, err)
	})
}
func (c *ahpConnection) cancelFileSearch(raw json.RawMessage) (any, error) {
	var p struct {
		SearchID string `json:"searchId"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil, ahpInvalid("invalid searchId")
	}
	c.searchMu.Lock()
	cancel := c.searches[p.SearchID]
	c.searchMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return struct{}{}, nil
}
