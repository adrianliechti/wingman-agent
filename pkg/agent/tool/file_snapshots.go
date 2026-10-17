package tool

import "encoding/json"

const FileSnapshotsMetadata = "file_snapshots"

// FileSnapshot records the bytes used to build a read-only edit preview.
// The harness consumes it privately; the model never copies a digest.
type FileSnapshot struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func ResultFileSnapshots(metadata map[string]any) ([]FileSnapshot, error) {
	value := metadata[FileSnapshotsMetadata]
	if value == nil {
		return nil, nil
	}
	if snapshots, ok := value.([]FileSnapshot); ok {
		return append([]FileSnapshot(nil), snapshots...), nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var snapshots []FileSnapshot
	err = json.Unmarshal(data, &snapshots)
	return snapshots, err
}
