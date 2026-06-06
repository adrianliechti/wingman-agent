package rewind

import (
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

type FileStatus int

const (
	StatusAdded FileStatus = iota
	StatusModified
	StatusDeleted
)

type FileDiff struct {
	Path   string
	Status FileStatus
	Patch  string

	Original string
	Modified string
}

func (m *Manager) DiffFromBaseline() ([]FileDiff, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, ErrClosed
	}

	if m.baselineHash.IsZero() {
		return nil, errors.New("no baseline available")
	}

	baselineCommit, err := m.repo.CommitObject(m.baselineHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get baseline commit: %w", err)
	}

	root, err := m.snapshot()
	if err != nil {
		return nil, fmt.Errorf("failed to snapshot working tree: %w", err)
	}

	if root == baselineCommit.TreeHash {
		return nil, nil
	}

	baselineTree, err := baselineCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get baseline tree: %w", err)
	}

	liveTree, err := object.GetTree(m.storage, root)
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshot tree: %w", err)
	}

	changes, err := object.DiffTree(baselineTree, liveTree)
	if err != nil {
		return nil, fmt.Errorf("failed to compute diff: %w", err)
	}

	var diffs []FileDiff

	for _, change := range changes {
		patch, err := change.Patch()
		if err != nil {
			continue
		}

		var status FileStatus
		var path string

		action, err := change.Action()
		if err != nil {
			continue
		}

		switch action {
		case merkletrie.Insert:
			status = StatusAdded
			path = change.To.Name
		case merkletrie.Delete:
			status = StatusDeleted
			path = change.From.Name
		case merkletrie.Modify:
			status = StatusModified
			path = change.To.Name
		default:
			continue
		}

		from, to, _ := change.Files()
		var original, modified string
		if from != nil {
			if c, err := from.Contents(); err == nil {
				original = c
			}
		}
		if to != nil {
			if c, err := to.Contents(); err == nil {
				modified = c
			}
		}

		diffs = append(diffs, FileDiff{
			Path:     path,
			Status:   status,
			Patch:    patch.String(),
			Original: original,
			Modified: modified,
		})
	}

	return diffs, nil
}
