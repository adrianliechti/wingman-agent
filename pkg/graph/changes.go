package graph

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/sergi/go-diff/diffmatchpatch"
)

type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
)

type fileChange struct {
	Path     string
	Kind     ChangeKind
	Lines    []int
	AllLines bool
}

type AffectedFile struct {
	File  string     `json:"file"`
	Kind  ChangeKind `json:"kind"`
	Nodes []*Node    `json:"nodes"`
}

type Changes struct {
	Files []AffectedFile `json:"files"`
}

func gitChanges(root string) ([]fileChange, error) {
	repo, err := git.PlainOpen(root)
	if err != nil {
		return nil, err
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, err
	}

	status, err := wt.Status()
	if err != nil {
		return nil, err
	}

	var headTree *object.Tree
	if ref, err := repo.Head(); err == nil {
		if commit, err := repo.CommitObject(ref.Hash()); err == nil {
			headTree, _ = commit.Tree()
		}
	}

	var out []fileChange
	for path := range status {
		slashPath := filepath.ToSlash(path)

		var headContent string
		inHead := false
		if headTree != nil {
			if f, err := headTree.File(path); err == nil {
				if c, err := f.Contents(); err == nil {
					headContent = c
					inHead = true
				}
			}
		}

		diskBytes, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		onDisk := readErr == nil

		switch {
		case !inHead && onDisk:
			out = append(out, fileChange{Path: slashPath, Kind: ChangeAdded, AllLines: true})
		case inHead && !onDisk:
			out = append(out, fileChange{Path: slashPath, Kind: ChangeDeleted})
		case inHead && onDisk:
			lines := changedNewLines(headContent, string(diskBytes))
			out = append(out, fileChange{Path: slashPath, Kind: ChangeModified, Lines: lines})
		}
	}

	return out, nil
}

func changedNewLines(oldText, newText string) []int {
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToChars(oldText, newText)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)

	var changed []int
	lineNo := 0
	for _, d := range diffs {
		n := countLines(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			lineNo += n
		case diffmatchpatch.DiffInsert:
			for i := 0; i < n; i++ {
				lineNo++
				changed = append(changed, lineNo)
			}
		case diffmatchpatch.DiffDelete:
			if lineNo >= 1 {
				changed = append(changed, lineNo)
			}
		}
	}
	return changed
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func affectedNodes(g *Graph, changes []fileChange) Changes {
	var result Changes

	for _, c := range changes {
		af := AffectedFile{File: c.Path, Kind: c.Kind}

		if c.Kind != ChangeDeleted {
			nodeset := map[string]*Node{}
			fileNodes := g.byFile[c.Path]

			if c.AllLines {
				for _, n := range fileNodes {
					nodeset[n.ID] = n
				}
			} else {
				lines := append([]int(nil), c.Lines...)
				sort.Ints(lines)
				for _, n := range fileNodes {
					i := sort.SearchInts(lines, n.StartLine)
					if i < len(lines) && lines[i] <= n.EndLine {
						nodeset[n.ID] = n
					}
				}
			}

			for _, n := range nodeset {
				af.Nodes = append(af.Nodes, n)
			}
			sort.SliceStable(af.Nodes, func(i, j int) bool {
				return af.Nodes[i].StartLine < af.Nodes[j].StartLine
			})
		}

		result.Files = append(result.Files, af)
	}

	sort.SliceStable(result.Files, func(i, j int) bool {
		return result.Files[i].File < result.Files[j].File
	})
	return result
}
