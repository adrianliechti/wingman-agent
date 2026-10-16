package graph

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	maxTracePaths     = 200
	maxTraceStates    = 10000
	maxTraceDepth     = 32
	TraceDisplayPaths = 40
	maxTraceEvidence  = 64
)

type TraceEvidence struct {
	File   string
	Line   int
	Source string
}

type TraceResult struct {
	Roots           []*Node
	Targets         []*Node
	Paths           []Path
	Depth           int
	Limited         bool
	SkippedFiles    []CoverageIssue
	Evidence        []TraceEvidence
	EvidenceOmitted bool
	SourceChanged   bool
}

func (e *Engine) Trace(ctx context.Context, from, to, file string, callers bool, maxDepth int) (TraceResult, error) {
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return TraceResult{}, err
	}
	result := TraceResult{Roots: g.resolve(from, file)}
	e.mu.RLock()
	files, matchingSnapshot := e.files, e.graph == g
	result.SkippedFiles = slices.Clone(e.skipped)
	e.mu.RUnlock()
	if len(result.Roots) == 0 {
		return result, g.notFoundErr(from, file)
	}
	if maxDepth <= 0 {
		maxDepth = 8
	}
	result.Depth = min(maxDepth, maxTraceDepth)
	targets := map[string]bool{}
	if to != "" {
		result.Targets = g.resolve(to, "")
		if len(result.Targets) == 0 {
			return result, g.notFoundErr(to, "")
		}
		for _, n := range result.Targets {
			targets[n.ID] = true
		}
	}
	budget := maxTraceStates
	for _, root := range result.Roots {
		paths, limited := g.trace(ctx, root.ID, targets, callers, result.Depth, maxTracePaths-len(result.Paths), &budget)
		result.Paths = append(result.Paths, paths...)
		result.Limited = result.Limited || limited
	}
	if err := ctx.Err(); err != nil {
		return TraceResult{}, err
	}
	seen := map[string]bool{}
	sources := map[string][]string{}
	for _, path := range result.Paths[:min(len(result.Paths), TraceDisplayPaths)] {
		for i := 0; i+1 < len(path.Nodes); i++ {
			from, to := path.Nodes[i], path.Nodes[i+1]
			if callers {
				from, to = to, from
			}
			line := g.edgeLine[from.ID+"\x00"+to.ID]
			key := fmt.Sprintf("%s:%d", from.File, line)
			if seen[key] {
				continue
			}
			seen[key] = true
			if line <= 0 || len(result.Evidence) >= maxTraceEvidence {
				result.EvidenceOmitted = true
				continue
			}
			lines, cached := sources[from.File]
			if !cached {
				meta, known := files[from.File]
				if !matchingSnapshot || !known {
					result.SourceChanged, result.EvidenceOmitted = true, true
					continue
				}
				data, changed, err := readTraceSource(filepath.Join(e.root, from.File), meta)
				result.SourceChanged = result.SourceChanged || changed
				if err == nil && !changed && len(data) <= maxFileBytes {
					lines = strings.Split(string(data), "\n")
				}
				sources[from.File] = lines
			}
			if line > len(lines) {
				result.EvidenceOmitted = true
				continue
			}
			source := strings.TrimSpace(lines[line-1])
			chars := []rune(source)
			if len(chars) > 320 {
				source = string(chars[:320]) + "…"
				result.EvidenceOmitted = true
			}
			result.Evidence = append(result.Evidence, TraceEvidence{File: from.File, Line: line, Source: source})
		}
	}
	return result, nil
}

// Targeted queries enumerate simple paths, including separate branches that
// merge before the target. Untargeted queries keep one route per reachable node.
// A shared state budget bounds exponential path growth and multiple roots.
func (g *Graph) trace(ctx context.Context, fromID string, targets map[string]bool, callers bool, depth, limit int, budget *int) ([]Path, bool) {
	if limit <= 0 || *budget <= 0 {
		return nil, true
	}
	adj := g.out
	if callers {
		adj = g.in
	}
	type frame struct {
		id   string
		path []string
	}
	queue := []frame{{fromID, []string{fromID}}}
	visited := map[string]bool{fromID: true}
	var paths []Path
	limited := false
	for head := 0; head < len(queue); head++ {
		if ctx.Err() != nil || *budget <= 0 {
			return paths, true
		}
		*budget -= 1
		cur := queue[head]
		for _, next := range adj[cur.id] {
			if g.byID[next] == nil || slices.Contains(cur.path, next) {
				continue
			}
			if len(targets) == 0 && visited[next] {
				continue
			}
			if len(cur.path)-1 >= depth {
				limited = true
				continue
			}
			nextPath := append(slices.Clone(cur.path), next)
			if len(targets) == 0 || targets[next] {
				if len(paths) >= limit {
					return paths, true
				}
				paths = append(paths, g.pathFromIDs(nextPath, callers))
			}
			if targets[next] {
				continue
			}
			if len(queue)-head >= *budget {
				limited = true
				continue
			}
			visited[next] = true
			queue = append(queue, frame{next, nextPath})
		}
	}
	return paths, limited
}

func readTraceSource(path string, expected fileMeta) ([]byte, bool, error) {
	return readSnapshotSource(path, expected, maxFileBytes)
}

// readSnapshotSource bounds the read and reports whether the disk source still
// matches the index. Changed content may be searched, but cannot use old ranges.
func readSnapshotSource(path string, expected fileMeta, maxBytes int64) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, true, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("not a regular source file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, true, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, true, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, true, fmt.Errorf("source is not a regular file of at most %d bytes", maxBytes)
	}
	changed := info.Size() != expected.Size || info.ModTime().UnixNano() != expected.MTime
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if int64(len(data)) > maxBytes {
		return nil, true, fmt.Errorf("source exceeds %d bytes", maxBytes)
	}
	after, statErr := file.Stat()
	if statErr != nil {
		return nil, true, statErr
	}
	if after.Size() != info.Size() || after.ModTime() != info.ModTime() {
		return nil, true, fmt.Errorf("source changed while reading; retry")
	}
	return data, changed, err
}
