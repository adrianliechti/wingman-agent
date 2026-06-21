package graph

import (
	"path/filepath"
	"sort"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Bounds on history traversal so co-change stays fast on large repos: stop
// after this many commits that touch the target, or after walking this many
// commits total, whichever comes first.
const (
	maxCoChangeHits   = 200
	maxCoChangeWalked = 5000
)

type CoChange struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}

type CoChangesResult struct {
	File    string     `json:"file"`
	Commits int        `json:"commits"`
	Related []CoChange `json:"related"`
}

func coChanges(root, target string, limit int) (CoChangesResult, error) {
	res := CoChangesResult{File: target}

	repo, err := git.PlainOpen(root)
	if err != nil {
		return res, err
	}
	head, err := repo.Head()
	if err != nil {
		return res, err
	}
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return res, err
	}
	defer iter.Close()

	counts := map[string]int{}
	walked := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if res.Commits >= maxCoChangeHits || walked >= maxCoChangeWalked {
			return storer.ErrStop
		}
		walked++

		files, err := commitFiles(c)
		if err != nil {
			return nil
		}
		if !files[target] {
			return nil
		}
		res.Commits++
		for f := range files {
			if f != target {
				counts[f]++
			}
		}
		return nil
	})
	if err != nil && err != storer.ErrStop {
		return res, err
	}

	for f, n := range counts {
		res.Related = append(res.Related, CoChange{File: f, Count: n})
	}
	sort.Slice(res.Related, func(i, j int) bool {
		if res.Related[i].Count != res.Related[j].Count {
			return res.Related[i].Count > res.Related[j].Count
		}
		return res.Related[i].File < res.Related[j].File
	})
	if limit > 0 && len(res.Related) > limit {
		res.Related = res.Related[:limit]
	}
	return res, nil
}

// commitFiles returns the set of paths changed by a commit relative to its
// first parent (the full file set for a root commit).
func commitFiles(c *object.Commit) (map[string]bool, error) {
	cur, err := c.Tree()
	if err != nil {
		return nil, err
	}

	files := map[string]bool{}
	if c.NumParents() == 0 {
		err := cur.Files().ForEach(func(f *object.File) error {
			files[filepath.ToSlash(f.Name)] = true
			return nil
		})
		return files, err
	}

	parent, err := c.Parent(0)
	if err != nil {
		return nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := parentTree.Diff(cur)
	if err != nil {
		return nil, err
	}
	for _, ch := range changes {
		name := ch.To.Name
		if name == "" {
			name = ch.From.Name
		}
		files[filepath.ToSlash(name)] = true
	}
	return files, nil
}
