package tool

import (
	"encoding/json"
	"path/filepath"
	"slices"
)

const FileChangesMetadata = "file_changes"

// FileChange is an exact, structured edit baseline. It is durable tool
// metadata; it is never included in the model-visible result or prompt.
type FileChange struct {
	Path         string          `json:"path"`
	Before       string          `json:"before"`
	After        string          `json:"after"`
	BeforeExists bool            `json:"beforeExists"`
	AfterExists  bool            `json:"afterExists"`
	Mode         uint32          `json:"mode"`
	AfterMode    *uint32         `json:"afterMode,omitempty"`
	BeforeRef    *FileContentRef `json:"beforeRef,omitempty"`
	AfterRef     *FileContentRef `json:"afterRef,omitempty"`
	MovedFrom    string          `json:"movedFrom,omitempty"`
}

// FileContentRef identifies unchanged bytes carried by a rename. The matching
// file supplies the bytes at undo time; the journal never embeds the payload.
type FileContentRef struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (r *FileContentRef) Equal(other *FileContentRef) bool {
	return r == other || r != nil && other != nil && *r == *other
}

// ResultMode is the post-edit mode; older records preserve Mode throughout.
func (c FileChange) ResultMode() uint32 {
	if c.AfterMode != nil {
		return *c.AfterMode
	}
	return c.Mode
}

// Unchanged reports whether undo has anything to restore at this path.
// Distinct files with identical bytes can still exchange paths.
func (c FileChange) Unchanged() bool {
	if !c.BeforeExists && !c.AfterExists {
		return true
	}
	return c.BeforeExists && c.AfterExists && c.MovedFrom == "" && c.Before == c.After && c.BeforeRef.Equal(c.AfterRef) && c.Mode == c.ResultMode()
}

// NetFileChanges folds each file's edits into one change, from the version the
// agent first found to the last one it wrote, keyed by the cleaned path. gap
// names the first file whose edit did not start where the previous one ended.
func NetFileChanges(changes []FileChange) (net []FileChange, gap string) {
	indexes := map[string]int{}
	origins := moveOrigins(changes)
	for i, change := range changes {
		path := filepath.Clean(filepath.FromSlash(change.Path))
		if origin := origins[i]; origin != "" {
			change.MovedFrom = origin
			if origin == path {
				change.MovedFrom = ""
			}
		}
		if index, ok := indexes[path]; ok {
			previous := &net[index]
			if gap == "" && (previous.After != change.Before || !previous.AfterRef.Equal(change.BeforeRef) || previous.AfterExists != change.BeforeExists || (change.BeforeExists && previous.ResultMode() != change.Mode)) {
				gap = path
			}
			previous.After, previous.AfterExists = change.After, change.AfterExists
			previous.AfterRef = change.AfterRef
			if change.AfterRef != nil || !change.AfterExists {
				previous.MovedFrom = change.MovedFrom
			}
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

// Each transaction reports one before/after pair per path. For a rename,
// the source row can occur on either side of the destination row, and both
// paths can be reused in later moves. Pair departures and arrivals per source
// in call order. A previous arrival gives the original path of a chained move.
func moveOrigins(changes []FileChange) map[int]string {
	if !slices.ContainsFunc(changes, func(c FileChange) bool { return c.AfterRef != nil && c.MovedFrom != "" }) {
		return nil
	}
	departures := make(map[string][]int)
	arrival := make(map[string]int)
	for i, change := range changes {
		path := filepath.Clean(filepath.FromSlash(change.Path))
		if change.BeforeRef != nil {
			departures[path] = append(departures[path], arrival[path])
		}
		if change.AfterRef != nil {
			arrival[path] = i + 1 // zero means the file predates the turn
		} else {
			delete(arrival, path)
		}
	}
	origins := make(map[int]string)
	for i, change := range changes {
		if change.MovedFrom == "" || change.AfterRef == nil {
			continue
		}
		source := filepath.Clean(filepath.FromSlash(change.MovedFrom))
		origins[i] = source
		if queue := departures[source]; len(queue) > 0 {
			prior := queue[0] - 1
			departures[source] = queue[1:]
			if origin := origins[prior]; origin != "" && changes[prior].AfterRef.Equal(change.AfterRef) {
				origins[i] = origin
			}
		}
	}
	return origins
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
