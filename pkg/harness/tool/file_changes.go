package tool

import (
	"encoding/json"
	"path/filepath"
)

const FileChangesMetadata = "file_changes"

// FileChange is an exact, structured edit baseline. It is durable tool
// metadata; it is never included in the model-visible result or prompt.
type FileChange struct {
	Path         string  `json:"path"`
	Before       string  `json:"before"`
	After        string  `json:"after"`
	BeforeExists bool    `json:"beforeExists"`
	AfterExists  bool    `json:"afterExists"`
	Mode         uint32  `json:"mode"`
	AfterMode    *uint32 `json:"afterMode,omitempty"`
}

// ResultMode is the post-edit mode; older records preserve Mode throughout.
func (c FileChange) ResultMode() uint32 {
	if c.AfterMode != nil {
		return *c.AfterMode
	}
	return c.Mode
}

// NetFileChanges folds each file's edits into one change, from the version the
// agent first found to the last one it wrote, keyed by the cleaned path. gap
// names the first file whose edit did not start where the previous one ended.
func NetFileChanges(changes []FileChange) (net []FileChange, gap string) {
	indexes := map[string]int{}
	for _, change := range changes {
		path := filepath.Clean(filepath.FromSlash(change.Path))
		if index, ok := indexes[path]; ok {
			previous := &net[index]
			if gap == "" && (previous.After != change.Before || previous.AfterExists != change.BeforeExists || (change.BeforeExists && previous.ResultMode() != change.Mode)) {
				gap = path
			}
			previous.After, previous.AfterExists = change.After, change.AfterExists
			mode := change.ResultMode()
			previous.AfterMode = nil
			if mode != previous.Mode {
				previous.AfterMode = &mode
			}
			continue
		}
		indexes[path] = len(net)
		change.Path = path
		net = append(net, change)
	}
	return net, gap
}

func ResultFileChanges(metadata map[string]any) ([]FileChange, error) {
	value := metadata[FileChangesMetadata]
	if value == nil {
		return nil, nil
	}
	if changes, ok := value.([]FileChange); ok {
		return append([]FileChange(nil), changes...), nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var changes []FileChange
	err = json.Unmarshal(data, &changes)
	return changes, err
}
