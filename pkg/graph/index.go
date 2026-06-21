package graph

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

const maxResolves = 2000

const maxFileBytes = 2 << 20

var skipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"target":       true,
	"out":          true,
	"bin":          true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".tox":         true,
	"testdata":     true,
}

type fileMeta struct {
	MTime int64 `json:"mtime"`
	Size  int64 `json:"size"`
}

type indexStats struct {
	Files int
	Nodes int
	Edges int
}

func indexRepo(ctx context.Context, root string, resolver CallResolver) (*Graph, map[string]fileMeta, indexStats, error) {
	taggers := map[string]*ts.Tagger{}
	files := map[string]fileMeta{}

	ax := newAuxExtractor()

	var nodes []*Node
	type ref struct {
		fromID string
		name   string
		file   string
		line   int
		col    int
		kind   EdgeKind
		lang   string
	}
	var refs []ref

	type fileImport struct {
		fromFile string
		norm     string
		rel      bool
	}
	var rawImports []fileImport

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || skipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		entry := grammars.DetectLanguage(name)
		if entry == nil {
			return nil
		}
		tagsQuery := grammars.ResolveTagsQuery(*entry)
		if tagsQuery == "" {
			return nil
		}
		if aug := tagsAugment[entry.Name]; aug != "" {
			tagsQuery += "\n" + aug
		}

		info, err := d.Info()
		if err != nil || info.Size() == 0 || info.Size() > maxFileBytes {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if looksMinified(src) {
			return nil
		}

		tagger := taggers[entry.Name]
		if tagger == nil {
			t, err := ts.NewTagger(entry.Language(), tagsQuery)
			if err != nil {
				return nil
			}
			tagger = t
			taggers[entry.Name] = t
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		files[rel] = fileMeta{MTime: info.ModTime().UnixNano(), Size: info.Size()}

		li := newLineIndex(src)
		tags := tagger.Tag(src)

		type defSpan struct {
			id    string
			start uint32
			end   uint32
			kind  Kind
		}
		var defs []defSpan

		for _, t := range tags {
			kind, ok := kindFromTag(t.Kind)
			if !ok {
				continue
			}
			id := fmt.Sprintf("%s#%s@%d", rel, t.Name, t.Range.StartByte)
			nodes = append(nodes, &Node{
				ID:        id,
				Kind:      kind,
				Name:      t.Name,
				File:      rel,
				StartLine: li.line(t.Range.StartByte),
				EndLine:   li.line(t.Range.EndByte),
				Lang:      entry.Name,
			})
			defs = append(defs, defSpan{id: id, start: t.Range.StartByte, end: t.Range.EndByte, kind: kind})
		}

		sort.Slice(defs, func(i, j int) bool {
			return (defs[i].end - defs[i].start) < (defs[j].end - defs[j].start)
		})

		enclosing := func(b uint32, typesOnly bool) string {
			for _, ds := range defs {
				if b < ds.start || b >= ds.end {
					continue
				}
				if typesOnly && !isTypeKind(ds.kind) {
					continue
				}
				return ds.id
			}
			return ""
		}

		for _, t := range tags {
			if !isCallTag(t.Kind) {
				continue
			}
			fromID := enclosing(t.Range.StartByte, false)
			if fromID == "" {
				continue
			}
			pos := t.NameRange.StartByte
			if t.NameRange.EndByte == 0 {
				pos = t.Range.StartByte
			}
			line, col := li.lspPos(pos)
			refs = append(refs, ref{fromID: fromID, name: t.Name, file: rel, line: line, col: col, kind: EdgeCalls, lang: entry.Name})
		}

		imps, hiers := ax.extract(entry.Name, entry.Language(), src)
		for _, im := range imps {
			rawImports = append(rawImports, fileImport{fromFile: rel, norm: im.norm, rel: im.rel})
		}
		for _, hr := range hiers {
			fromID := enclosing(hr.startByte, true)
			if fromID == "" {
				continue
			}
			line, col := li.lspPos(hr.startByte)
			refs = append(refs, ref{fromID: fromID, name: hr.name, file: rel, line: line, col: col, kind: hr.kind, lang: entry.Name})
		}

		return nil
	})

	if walkErr != nil {
		return nil, nil, indexStats{}, walkErr
	}

	g := &Graph{Nodes: nodes}
	g.build()

	seen := map[string]bool{}
	addEdge := func(from, to string, kind EdgeKind, via Provenance) {
		if from == to {
			return
		}
		key := from + "\x00" + to + "\x00" + string(kind)
		if seen[key] {
			return
		}
		seen[key] = true
		g.Edges = append(g.Edges, &Edge{From: from, To: to, Kind: kind, Via: via})
	}

	resolves := 0
	processed := make(map[string]bool, len(refs))
	for _, r := range refs {
		pk := r.fromID + "\x00" + r.name + "\x00" + string(r.kind)
		if processed[pk] {
			continue
		}
		processed[pk] = true

		var cands []*Node
		for _, c := range g.byName[r.name] {
			if c.Lang == r.lang {
				cands = append(cands, c)
			}
		}
		switch len(cands) {
		case 0:
			continue
		case 1:
			addEdge(r.fromID, cands[0].ID, r.kind, ViaName)
		default:
			if resolver != nil && resolves < maxResolves {
				resolves++
				if df, dl, ok := resolver.ResolveCall(ctx, r.file, r.line, r.col); ok {
					if tgt := g.nodeAt(df, dl); tgt != nil {
						addEdge(r.fromID, tgt.ID, r.kind, ViaLSP)
						continue
					}
				}
			}
			for _, cand := range cands {
				addEdge(r.fromID, cand.ID, r.kind, ViaAmbiguous)
			}
		}
	}

	localDirs := make(map[string]bool, len(files))
	for f := range files {
		localDirs[path.Dir(f)] = true
	}
	for _, ri := range rawImports {
		g.Imports = append(g.Imports, &Import{
			FromFile: ri.fromFile,
			Path:     ri.norm,
			ToModule: resolveImport(ri.fromFile, ri.norm, ri.rel, localDirs),
		})
	}

	g.build()

	stats := indexStats{Files: len(files), Nodes: len(g.Nodes), Edges: len(g.Edges)}
	return g, files, stats, nil
}

func looksMinified(src []byte) bool {
	const maxLine = 2000
	last := -1
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			if i-last-1 > maxLine {
				return true
			}
			last = i
		}
	}
	return len(src)-last-1 > maxLine
}

type lineIndex struct {
	src    []byte
	starts []uint32
}

func newLineIndex(src []byte) *lineIndex {
	starts := []uint32{0}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			starts = append(starts, uint32(i+1))
		}
	}
	return &lineIndex{src: src, starts: starts}
}

func (l *lineIndex) line(byteOff uint32) int {
	lo, hi := 0, len(l.starts)
	for lo < hi {
		mid := (lo + hi) / 2
		if l.starts[mid] <= byteOff {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func (l *lineIndex) lspPos(byteOff uint32) (line, column int) {
	ln := l.line(byteOff)
	return ln - 1, utf16Len(l.src[l.starts[ln-1]:byteOff])
}

func utf16Len(b []byte) int {
	n := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
		b = b[size:]
	}
	return n
}
