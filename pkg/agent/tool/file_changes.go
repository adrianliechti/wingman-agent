package tool

import "encoding/json"

const FileChangesMetadata = "file_changes"

// FileChange is an exact, structured edit baseline. It is durable tool
// metadata; it is never included in the model-visible result or prompt.
type FileChange struct {
	Path         string `json:"path"`
	Before       string `json:"before"`
	After        string `json:"after"`
	BeforeExists bool   `json:"beforeExists"`
	AfterExists  bool   `json:"afterExists"`
	Mode         uint32 `json:"mode"`
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
