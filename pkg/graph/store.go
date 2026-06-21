package graph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type snapshot struct {
	Version   int                 `json:"version"`
	IndexedAt time.Time           `json:"indexed_at"`
	Files     map[string]fileMeta `json:"files"`
	Nodes     []*Node             `json:"nodes"`
	Edges     []*Edge             `json:"edges"`
	Imports   []*Import           `json:"imports,omitempty"`
}

const snapshotVersion = 2

func loadSnapshot(path string) (*Graph, map[string]fileMeta, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, time.Time{}, err
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, nil, time.Time{}, err
	}
	if snap.Version != snapshotVersion {
		return nil, nil, time.Time{}, os.ErrNotExist
	}

	g := &Graph{Nodes: snap.Nodes, Edges: snap.Edges, Imports: snap.Imports}
	g.build()
	return g, snap.Files, snap.IndexedAt, nil
}

func saveSnapshot(path string, g *Graph, files map[string]fileMeta, indexedAt time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	snap := snapshot{
		Version:   snapshotVersion,
		IndexedAt: indexedAt,
		Files:     files,
		Nodes:     g.Nodes,
		Edges:     g.Edges,
		Imports:   g.Imports,
	}

	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
