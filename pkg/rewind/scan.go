package rewind

import (
	"encoding/binary"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

type scanEntry struct {
	path  string
	size  int64
	mtime int64
	mode  filemode.FileMode
}

type scanResult struct {
	start      time.Time
	entries    []scanEntry
	submodules []string
	sum        uint64
}

func (m *Manager) scan() *scanResult {
	s := &scanner{
		matcher: m.excludeMatcher(),
		sem:     make(chan struct{}, runtime.NumCPU()),
	}

	start := time.Now()
	s.scanDir(m.workingDir, nil)

	return &scanResult{
		start:      start,
		entries:    s.entries,
		submodules: s.submodules,
		sum:        s.sum,
	}
}

type scanner struct {
	matcher gitignore.Matcher
	sem     chan struct{}

	mu         sync.Mutex
	entries    []scanEntry
	submodules []string
	sum        uint64
}

func (s *scanner) scanDir(dir string, components []string) {
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	if len(components) > 0 && slices.ContainsFunc(dirents, func(e fs.DirEntry) bool { return e.Name() == ".git" }) {
		s.mu.Lock()
		s.submodules = append(s.submodules, strings.Join(components, "/"))
		s.mu.Unlock()
		return
	}

	var local []scanEntry
	var sum uint64
	var wg sync.WaitGroup

	for _, e := range dirents {
		name := e.Name()
		if len(components) == 0 && name == ".git" {
			continue
		}

		child := append(components, name)

		if e.IsDir() {
			if s.matcher.Match(child, true) {
				continue
			}
			sub := filepath.Join(dir, name)
			select {
			case s.sem <- struct{}{}:
				comps := slices.Clone(child)
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.scanDir(sub, comps)
					<-s.sem
				}()
			default:
				s.scanDir(sub, child)
			}
			continue
		}

		if s.matcher.Match(child, false) {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		mode := filemode.Regular
		switch fm := info.Mode(); {
		case fm&fs.ModeSymlink != 0:
			mode = filemode.Symlink
		case !fm.IsRegular():
			continue
		case fm&0o111 != 0:
			mode = filemode.Executable
		}

		local = append(local, scanEntry{
			path:  strings.Join(child, "/"),
			size:  info.Size(),
			mtime: info.ModTime().UnixNano(),
			mode:  mode,
		})
		sum ^= hashEntry(child, info)
	}

	wg.Wait()

	s.mu.Lock()
	s.entries = append(s.entries, local...)
	s.sum ^= sum
	s.mu.Unlock()
}

func hashEntry(components []string, info fs.FileInfo) uint64 {
	h := fnv.New64a()
	for i, c := range components {
		if i > 0 {
			h.Write([]byte{filepath.Separator})
		}
		h.Write([]byte(c))
	}
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], uint64(info.ModTime().UnixNano()))
	binary.LittleEndian.PutUint64(buf[8:], uint64(info.Size()))
	h.Write(buf[:])
	return h.Sum64()
}
