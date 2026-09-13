package fs

import (
	"context"
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
	st := fileState{target: target, modTime: info.ModTime(), size: info.Size()}
	f.mu.Lock()
	defer f.mu.Unlock()
	if origin := tool.BackgroundOrigin(ctx); origin != "" {
		if f.background[origin] == nil {
			f.background[origin] = map[string]fileState{}
		}
		f.background[origin][target.AbsPath] = st
		return
	}
	f.states[target.AbsPath] = st
	f.announced[target.AbsPath] = st
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
