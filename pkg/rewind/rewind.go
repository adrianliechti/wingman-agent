package rewind

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

var ErrClosed = errors.New("rewind manager closed")

type Checkpoint struct {
	Hash    string
	Message string
	Time    time.Time
}

type Manager struct {
	workingDir string

	initDone chan struct{}
	initErr  error

	mu           sync.Mutex
	storeMu      sync.Mutex
	repo         *git.Repository
	storage      *readThroughStorage
	gitDir       string
	baselineHash plumbing.Hash
	manifest     map[string]manifestEntry
	floor        int64
	closed       bool

	excludesMu      sync.Mutex
	excludesMatcher gitignore.Matcher
}

func New(workingDir string) *Manager {
	m := &Manager{
		workingDir: workingDir,
		initDone:   make(chan struct{}),
	}
	go m.init()
	return m
}

func CleanupOrphans() {
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "wingman-rewind-*"))
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.RemoveAll(m)
		}
	}
}

func (m *Manager) init() {
	defer close(m.initDone)

	sessionID := fmt.Sprintf("%d", time.Now().UnixNano())
	gitDir := filepath.Join(os.TempDir(), "wingman-rewind-"+sessionID)

	if err := os.MkdirAll(gitDir, 0755); err != nil {
		m.initErr = fmt.Errorf("failed to create git dir: %w", err)
		return
	}
	m.gitDir = gitDir

	var userStorer storer.EncodedObjectStorer
	var userHead *object.Commit
	var userIndex *index.Index
	if userRepo, err := git.PlainOpen(m.workingDir); err == nil {
		userStorer = userRepo.Storer
		if ref, err := userRepo.Head(); err == nil {
			if c, err := userRepo.CommitObject(ref.Hash()); err == nil {
				userHead = c
			}
		}
		if idx, err := userRepo.Storer.Index(); err == nil {
			userIndex = idx
		}
	}

	m.storage = &readThroughStorage{
		Storer:    filesystem.NewStorage(osfs.New(gitDir), cache.NewObjectLRUDefault()),
		secondary: userStorer,
	}

	repo, err := git.Init(m.storage, nil)
	if err != nil {
		m.initErr = fmt.Errorf("failed to init repo: %w", err)
		return
	}

	m.repo = repo
	m.manifest = map[string]manifestEntry{}

	if userHead != nil {
		if err := m.baselineFromHEAD(userHead, userIndex); err != nil {
			m.initErr = fmt.Errorf("failed to create baseline: %w", err)
		}
		return
	}

	if err := m.baselineFromWorkingTree(); err != nil {
		m.initErr = fmt.Errorf("failed to create baseline: %w", err)
	}
}

func (m *Manager) ready() error {
	<-m.initDone
	return m.initErr
}

func (m *Manager) absPath(name string) string {
	return filepath.Join(m.workingDir, filepath.FromSlash(name))
}

func (m *Manager) commitBaseline(tree plumbing.Hash) error {
	hash, err := m.writeCommit("Session Start", tree)
	if err != nil {
		return err
	}

	if err := m.setHead(hash); err != nil {
		return err
	}

	m.baselineHash = hash
	return nil
}

func (m *Manager) baselineFromHEAD(headCommit *object.Commit, idx *index.Index) error {
	if err := m.commitBaseline(headCommit.TreeHash); err != nil {
		return err
	}

	if idx != nil {
		dups := map[string]bool{}
		for _, e := range idx.Entries {
			if e.SkipWorktree || e.IntentToAdd {
				continue
			}
			if dups[e.Name] {
				continue
			}
			if _, ok := m.manifest[e.Name]; ok {
				dups[e.Name] = true
				delete(m.manifest, e.Name)
				continue
			}
			m.manifest[e.Name] = manifestEntry{
				size:  int64(e.Size),
				mtime: e.ModifiedAt.UnixNano(),
				mode:  e.Mode,
				hash:  e.Hash,
			}
		}
	}

	m.floor = time.Now().UnixNano()
	return nil
}

func (m *Manager) baselineFromWorkingTree() error {
	root, err := m.snapshot()
	if err != nil {
		return err
	}

	return m.commitBaseline(root)
}

func (m *Manager) writeCommit(message string, tree plumbing.Hash, parents ...plumbing.Hash) (plumbing.Hash, error) {
	sig := object.Signature{Name: "wingman", Email: "wingman@local", When: time.Now()}
	commit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      message,
		TreeHash:     tree,
		ParentHashes: parents,
	}

	obj := m.storage.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to encode commit: %w", err)
	}

	hash, err := m.storage.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to write commit: %w", err)
	}

	return hash, nil
}

func (m *Manager) setHead(hash plumbing.Hash) error {
	branch := plumbing.NewBranchReferenceName("master")
	if err := m.repo.Storer.SetReference(plumbing.NewHashReference(branch, hash)); err != nil {
		return fmt.Errorf("failed to set branch ref: %w", err)
	}
	if err := m.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
		return fmt.Errorf("failed to set HEAD: %w", err)
	}
	return nil
}

func (m *Manager) excludeMatcher() gitignore.Matcher {
	m.excludesMu.Lock()
	defer m.excludesMu.Unlock()
	if m.excludesMatcher == nil {
		m.excludesMatcher = gitignore.NewMatcher(m.loadExcludes())
	}
	return m.excludesMatcher
}

func (m *Manager) refreshExcludes() {
	m.excludesMu.Lock()
	m.excludesMatcher = gitignore.NewMatcher(m.loadExcludes())
	m.excludesMu.Unlock()
}

func (m *Manager) loadExcludes() []gitignore.Pattern {
	patterns, _ := gitignore.ReadPatterns(osfs.New(m.workingDir), nil)

	rootFS := osfs.New("/")
	if global, err := gitignore.LoadGlobalPatterns(rootFS); err == nil {
		patterns = append(patterns, global...)
	}
	if system, err := gitignore.LoadSystemPatterns(rootFS); err == nil {
		patterns = append(patterns, system...)
	}

	return append(patterns, readXDGIgnore()...)
}

func readXDGIgnore() []gitignore.Pattern {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		xdg = filepath.Join(home, ".config")
	}

	f, err := os.Open(filepath.Join(xdg, "git", "ignore"))
	if err != nil {
		return nil
	}
	defer f.Close()

	var ps []gitignore.Pattern
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || len(strings.TrimSpace(line)) == 0 {
			continue
		}
		ps = append(ps, gitignore.ParsePattern(line, nil))
	}
	return ps
}

func (m *Manager) Commit(message string) error {
	if err := m.ready(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}

	root, err := m.snapshot()
	if err != nil {
		return fmt.Errorf("failed to snapshot working tree: %w", err)
	}

	headRef, err := m.repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	headCommit, err := m.repo.CommitObject(headRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get HEAD commit: %w", err)
	}

	if headCommit.TreeHash == root {
		return nil
	}

	hash, err := m.writeCommit(message, root, headRef.Hash())
	if err != nil {
		return err
	}

	return m.setHead(hash)
}

func (m *Manager) List() ([]Checkpoint, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, ErrClosed
	}

	ref, err := m.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	iter, err := m.repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, fmt.Errorf("failed to get log: %w", err)
	}

	var checkpoints []Checkpoint

	err = iter.ForEach(func(c *object.Commit) error {
		checkpoints = append(checkpoints, Checkpoint{
			Hash:    c.Hash.String(),
			Message: c.Message,
			Time:    c.Author.When,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate commits: %w", err)
	}

	return checkpoints, nil
}

func (m *Manager) Cleanup() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-m.initDone
		m.mu.Lock()
		defer m.mu.Unlock()
		m.closed = true
		if m.gitDir != "" {
			os.RemoveAll(m.gitDir)
			m.gitDir = ""
		}
	}()

	select {
	case <-done:
	case <-time.After(cleanupTimeout):
	}
}

var cleanupTimeout = 5 * time.Second

func (m *Manager) Fingerprint() uint64 {
	if err := m.ready(); err != nil {
		return 0
	}
	return m.scan().sum
}
