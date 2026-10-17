package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type fileState struct {
	target  fileTarget
	modTime time.Time
	size    int64
	missing bool
	digest  string
}

// Freshness tracks the on-disk state of files the session's file tools
// touched, so external modifications (user edits, linters, shell commands)
// can be detected and announced to the model. A nil Freshness disables it.
type Freshness struct {
	root *os.Root

	mu         sync.Mutex
	states     map[string]fileState
	announced  map[string]fileState
	background map[string]map[string]fileState
}

func NewFreshness(root *os.Root) *Freshness {
	return &Freshness{root: root, states: map[string]fileState{}, announced: map[string]fileState{}, background: map[string]map[string]fileState{}}
}

// Each agent has its own read baseline. Change notifications never advance it.
func (f *Freshness) record(ctx context.Context, target fileTarget) {
	if f == nil {
		return
	}
	info, err := statFileTarget(f.root, target)
	if err == nil {
		f.recordInfo(ctx, target, info)
	}
}

func (f *Freshness) recordInfo(ctx context.Context, target fileTarget, info os.FileInfo) {
	if f == nil || info == nil || info.IsDir() {
		return
	}
	f.recordState(ctx, fileState{target: target, modTime: info.ModTime(), size: info.Size()})
}

func (f *Freshness) recordState(ctx context.Context, st fileState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if origin := tool.BackgroundOrigin(ctx); origin != "" {
		if f.background[origin] == nil {
			f.background[origin] = map[string]fileState{}
		}
		f.background[origin][st.target.AbsPath] = st
		return
	}
	f.states[st.target.AbsPath] = st
	f.announced[st.target.AbsPath] = st
}

// TrackSnapshots connects trusted preview tools to the session's edit baseline.
// Ordinary reads deliberately refresh this baseline, as they do for stat-based
// freshness. Each background agent keeps its own baseline.
func (f *Freshness) TrackSnapshots(t tool.Tool) tool.Tool {
	if f == nil {
		return t
	}
	execute := t.Execute
	t.Execute = func(ctx context.Context, args map[string]any) (tool.Result, error) {
		result, err := execute(ctx, args)
		if err != nil || result.IsError {
			return result, err
		}
		snapshots, err := tool.ResultFileSnapshots(result.Metadata)
		if err != nil {
			return tool.Result{}, fmt.Errorf("read preview snapshots: %w", err)
		}
		var states []fileState
		for _, snapshot := range snapshots {
			digest, err := hex.DecodeString(snapshot.SHA256)
			if err != nil || len(digest) != sha256.Size {
				return tool.Result{}, fmt.Errorf("invalid preview snapshot for %s", snapshot.Path)
			}
			target, err := resolveFileTarget(snapshot.Path, f.root.Name(), nil, "track preview")
			if err != nil {
				return tool.Result{}, err
			}
			info, err := statFileTarget(f.root, target)
			if err != nil {
				return tool.Result{}, fmt.Errorf("preview file changed; refresh the preview: %w", err)
			}
			states = append(states, fileState{target: target, modTime: info.ModTime(), size: info.Size(), digest: hex.EncodeToString(digest)})
		}
		for _, st := range states {
			f.recordState(ctx, st)
		}
		return result, nil
	}
	return t
}

func (f *Freshness) staleContent(ctx context.Context, target fileTarget, content []byte) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	states := f.states
	if origin := tool.BackgroundOrigin(ctx); origin != "" {
		states = f.background[origin]
	}
	digest := states[target.AbsPath].digest
	f.mu.Unlock()
	return digest != "" && fmt.Sprintf("%x", sha256.Sum256(content)) != digest
}

func (f *Freshness) forget(ctx context.Context, target fileTarget) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if origin := tool.BackgroundOrigin(ctx); origin != "" {
		delete(f.background[origin], target.AbsPath)
		return
	}
	delete(f.states, target.AbsPath)
	delete(f.announced, target.AbsPath)
}

func (f *Freshness) stale(ctx context.Context, target fileTarget, info os.FileInfo) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	states := f.states
	if origin := tool.BackgroundOrigin(ctx); origin != "" {
		states = f.background[origin]
	}
	st, ok := states[target.AbsPath]
	return ok && (!info.ModTime().Equal(st.modTime) || info.Size() != st.size)
}

// Changed stats every tracked file and returns the paths whose on-disk state
// changed since its last notification. Read baselines remain unchanged until
// the agent reads the file again. Deleted paths remain tracked for recreation.
func (f *Freshness) Changed() []string {
	if f == nil {
		return nil
	}

	f.mu.Lock()
	states := make(map[string]fileState, len(f.announced))
	maps.Copy(states, f.announced)
	f.mu.Unlock()

	var changed []string
	for key, st := range states {
		info, err := statFileTarget(f.root, st.target)

		f.mu.Lock()
		current, ok := f.announced[key]
		if !ok || current.modTime != st.modTime || current.size != st.size || current.missing != st.missing {
			// A tool updated the record while we were sweeping — that change
			// is the session's own.
			f.mu.Unlock()
			continue
		}
		switch {
		case os.IsNotExist(err):
			if !st.missing {
				st.missing = true
				f.announced[key] = st
				changed = append(changed, key+" (deleted)")
			}
		case err != nil:
			// A transient stat error is not a deletion; retry on the next sweep.
		case st.missing || !info.ModTime().Equal(st.modTime) || info.Size() != st.size:
			f.announced[key] = fileState{target: st.target, modTime: info.ModTime(), size: info.Size()}
			changed = append(changed, key)
		}
		f.mu.Unlock()
	}

	slices.Sort(changed)
	return changed
}
