package fs

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	pathpkg "path"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const grepResultBudget = tool.MaxGrepResultBytes - 1024

var errGrepPageFull = errors.New("grep page complete")

type grepPage struct {
	skip, limit          int
	entries              []string
	bytes                int
	truncated, byteLimit bool
	err                  error
}

func (p *grepPage) add(entry string) bool {
	if p.skip > 0 {
		p.skip--
		return true
	}
	if len(p.entries) >= p.limit {
		p.truncated = true
		return false
	}
	if len(entry)+1 > grepResultBudget-p.bytes {
		p.truncated, p.byteLimit = true, true
		if len(p.entries) == 0 {
			p.err = fmt.Errorf("a search result exceeds the output byte limit; use a shorter search path")
		}
		return false
	}
	p.entries = append(p.entries, entry)
	p.bytes += len(entry) + 1
	return true
}

func grepSaturatingAdd(a, b int) int {
	if b > math.MaxInt-a {
		return math.MaxInt
	}
	return a + b
}

func executeGrep(ctx context.Context, root *os.Root, cfg *grepArgs, allowedReadRoots []string) (tool.Result, error) {
	target, err := resolveSearchTarget(cfg.searchPath, root.Name(), root, allowedReadRoots, "search")
	if err != nil {
		return tool.Result{}, err
	}
	defer target.Close()
	info, err := target.Root.Stat(target.SearchDirFS)
	if err != nil {
		return tool.Result{}, fmt.Errorf("stat path %q: %w", cfg.searchPath, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return tool.Result{}, fmt.Errorf("path is not a regular file: %s", cfg.searchPath)
	}
	accepts := func(path, relative string) bool {
		return matchesGrepGlobs(cfg.globPatterns, path, relative) && (cfg.typeFilter == "" || matchesType(path, cfg.typeFilter))
	}
	if !info.IsDir() && !accepts(target.SearchDirFS, pathpkg.Base(target.SearchDirFS)) {
		return tool.Text("No matches found"), nil
	}
	fsys := target.Root.FS()
	page := &grepPage{skip: cfg.resultOffset, limit: cfg.effectiveLimit}
	var files globResults
	// A page cannot contain more entries than bytes. Keep only enough newest
	// files for the requested offset, the page, and one extra to detect truncation.
	retain := grepSaturatingAdd(cfg.resultOffset, min(cfg.effectiveLimit, grepResultBudget)+1)
	totalOccurrences := 0
	visit := func(path, relative string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !accepts(path, relative) {
			return nil
		}
		display := target.ReportPath(path)
		switch cfg.outputMode {
		case "files_with_matches":
			matched, err := fileHasMatch(ctx, fsys, path, cfg.re, cfg.multiline)
			if err != nil {
				return fmt.Errorf("search %s: %w", display, err)
			}
			if matched {
				info, err := d.Info()
				if err != nil {
					return err
				}
				item := globFile{path: display, modTime: info.ModTime()}
				if len(files) < retain {
					heap.Push(&files, item)
				} else if compareGlobFiles(item, files[0]) < 0 {
					files[0] = item
					heap.Fix(&files, 0)
				}
			}
		case "count":
			count, err := countFileMatches(ctx, fsys, path, cfg.re, cfg.multiline)
			if err != nil {
				return fmt.Errorf("search %s: %w", display, err)
			}
			if count > 0 {
				before := len(page.entries)
				if !page.add(fmt.Sprintf("%s:%d", display, count)) {
					return errGrepPageFull
				}
				if len(page.entries) > before {
					totalOccurrences += count
				}
			}
		default:
			if err := searchFileWithContext(ctx, fsys, path, display, cfg, page.add); err != nil {
				return fmt.Errorf("search %s: %w", display, err)
			}
		}
		return nil
	}
	if info.IsDir() {
		err = walkGrepFiles(ctx, fsys, target.SearchDirFS, func(path, relative string, d fs.DirEntry) error {
			err := visit(path, relative, d)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		})
	} else {
		err = visit(target.SearchDirFS, pathpkg.Base(target.SearchDirFS), fs.FileInfoToDirEntry(info))
	}
	if err != nil && !errors.Is(err, errGrepPageFull) {
		return tool.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	if cfg.outputMode == "files_with_matches" {
		slices.SortFunc(files, compareGlobFiles)
		for _, file := range files {
			if !page.add(file.path) {
				break
			}
		}
	}
	if page.err != nil {
		return tool.Result{}, page.err
	}
	if len(page.entries) == 0 {
		if cfg.outputMode == "files_with_matches" {
			return tool.Text("No files found"), nil
		}
		return tool.Text("No matches found"), nil
	}
	output := strings.Join(page.entries, "\n")
	limitInfo := formatGrepLimitInfo(page.truncated, cfg.headLimit, cfg.resultOffset)
	switch cfg.outputMode {
	case "files_with_matches":
		output = fmt.Sprintf("Found %d %s%s\n%s", len(page.entries), plural(len(page.entries), "file"), limitInfo, output)
	case "count":
		output += fmt.Sprintf("\n\nFound %d total %s across %d %s.", totalOccurrences, plural(totalOccurrences, "occurrence"), len(page.entries), plural(len(page.entries), "file"))
	}
	if page.truncated || cfg.resultOffset > 0 {
		notice := formatGrepPaginationNotice(page.truncated, cfg.headLimit, cfg.resultOffset)
		if page.byteLimit {
			notice += "; output byte limit reached"
		}
		if page.truncated {
			notice += fmt.Sprintf("; continue with skip: %d", grepSaturatingAdd(cfg.resultOffset, len(page.entries)))
		}
		output += "\n\n[" + notice + "]"
	}
	return tool.Text(output), nil
}
