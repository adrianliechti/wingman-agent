package rewind

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/utils/merkletrie"
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
	repo         *git.Repository
	worktree     *git.Worktree
	gitDir       string
	baselineHash plumbing.Hash
	closed       bool

	excludesOnce    sync.Once
	excludesPattern []gitignore.Pattern
	excludesMatcher gitignore.Matcher
}

type readThroughStorage struct {
	storage.Storer
	secondary storer.EncodedObjectStorer
}

func (s *readThroughStorage) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	obj, err := s.Storer.EncodedObject(t, h)
	if err == nil {
		return obj, nil
	}
	if errors.Is(err, plumbing.ErrObjectNotFound) && s.secondary != nil {
		return s.secondary.EncodedObject(t, h)
	}
	return obj, err
}

func (s *readThroughStorage) HasEncodedObject(h plumbing.Hash) error {
	if err := s.Storer.HasEncodedObject(h); err == nil {
		return nil
	} else if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return err
	}
	if s.secondary != nil {
		return s.secondary.HasEncodedObject(h)
	}
	return plumbing.ErrObjectNotFound
}

func (s *readThroughStorage) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	size, err := s.Storer.EncodedObjectSize(h)
	if err == nil {
		return size, nil
	}
	if errors.Is(err, plumbing.ErrObjectNotFound) && s.secondary != nil {
		return s.secondary.EncodedObjectSize(h)
	}
	return size, err
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
	if userRepo, err := git.PlainOpen(m.workingDir); err == nil {
		userStorer = userRepo.Storer
		if ref, err := userRepo.Head(); err == nil {
			if c, err := userRepo.CommitObject(ref.Hash()); err == nil {
				userHead = c
			}
		}
	}

	gitDirFS := osfs.New(gitDir)
	workTreeFS := osfs.New(m.workingDir)
	tempStorage := filesystem.NewStorage(gitDirFS, cache.NewObjectLRUDefault())
	rewindStorage := &readThroughStorage{
		Storer:    tempStorage,
		secondary: userStorer,
	}

	repo, err := git.Init(rewindStorage, nil)
	if err != nil {
		m.initErr = fmt.Errorf("failed to init repo: %w", err)
		return
	}

	cfg, err := repo.Config()
	if err != nil {
		m.initErr = fmt.Errorf("failed to get config: %w", err)
		return
	}
	cfg.Core.Worktree = m.workingDir
	if err := repo.SetConfig(cfg); err != nil {
		m.initErr = fmt.Errorf("failed to set config: %w", err)
		return
	}

	repo, err = git.Open(rewindStorage, workTreeFS)
	if err != nil {
		m.initErr = fmt.Errorf("failed to open repo: %w", err)
		return
	}

	worktree, err := repo.Worktree()
	if err != nil {
		m.initErr = fmt.Errorf("failed to get worktree: %w", err)
		return
	}

	m.repo = repo
	m.worktree = worktree

	if userHead != nil {
		if err := m.baselineFromHEAD(userHead); err != nil {
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

func (m *Manager) baselineFromHEAD(headCommit *object.Commit) error {
	sig := object.Signature{Name: "wingman", Email: "wingman@local", When: time.Now()}
	baselineCommit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   "Session Start",
		TreeHash:  headCommit.TreeHash,
	}

	obj := m.repo.Storer.NewEncodedObject()
	if err := baselineCommit.Encode(obj); err != nil {
		return fmt.Errorf("failed to encode baseline: %w", err)
	}

	hash, err := m.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return fmt.Errorf("failed to write baseline: %w", err)
	}

	if err := m.setHead(hash); err != nil {
		return err
	}

	m.baselineHash = hash
	return nil
}

func (m *Manager) baselineFromWorkingTree() error {
	m.worktree.Excludes = m.excludes()

	if err := m.worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("failed to stage baseline: %w", err)
	}

	hash, err := m.worktree.Commit("Session Start", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "wingman",
			Email: "wingman@local",
			When:  time.Now(),
		},
		AllowEmptyCommits: true,
	})
	if err != nil {
		return fmt.Errorf("failed to commit baseline: %w", err)
	}

	m.baselineHash = hash
	return nil
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

func (m *Manager) excludes() []gitignore.Pattern {
	m.excludesOnce.Do(m.computeExcludes)
	return m.excludesPattern
}

func (m *Manager) excludeMatcher() gitignore.Matcher {
	m.excludesOnce.Do(m.computeExcludes)
	return m.excludesMatcher
}

func (m *Manager) computeExcludes() {
	patterns, _ := gitignore.ReadPatterns(m.worktree.Filesystem, nil)

	rootFS := osfs.New("/")
	if global, err := gitignore.LoadGlobalPatterns(rootFS); err == nil {
		patterns = append(patterns, global...)
	}
	if system, err := gitignore.LoadSystemPatterns(rootFS); err == nil {
		patterns = append(patterns, system...)
	}

	patterns = append(patterns, readXDGIgnore()...)

	m.excludesPattern = patterns
	m.excludesMatcher = gitignore.NewMatcher(patterns)
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

	m.worktree.Excludes = m.excludes()

	status, err := m.worktree.Status()
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}
	if status.IsClean() {
		return nil
	}

	if err := m.worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("failed to add files: %w", err)
	}

	if _, err := m.worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "wingman",
			Email: "wingman@local",
			When:  time.Now(),
		},
		AllowEmptyCommits: false,
	}); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	return nil
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

func (m *Manager) Restore(hash string) error {
	if hash == "" {
		return errors.New("empty hash")
	}

	if err := m.ready(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}

	m.worktree.Excludes = m.excludes()

	if err := m.worktree.Clean(&git.CleanOptions{
		Dir: true,
	}); err != nil {
		return fmt.Errorf("failed to clean worktree: %w", err)
	}

	target := plumbing.NewHash(hash)
	if err := m.worktree.Checkout(&git.CheckoutOptions{
		Hash:  target,
		Force: true,
	}); err != nil {
		return fmt.Errorf("failed to checkout: %w", err)
	}

	if err := m.setHead(target); err != nil {
		return err
	}

	m.baselineHash = target
	return nil
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

	s := &fpScanner{
		matcher: m.excludeMatcher(),
		sem:     make(chan struct{}, runtime.NumCPU()),
	}
	return s.scanDir(m.workingDir, nil)
}

type fpScanner struct {
	matcher gitignore.Matcher
	sem     chan struct{}
}

func (s *fpScanner) scanDir(dir string, components []string) uint64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	var sum, asyncSum uint64
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, e := range entries {
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
					v := s.scanDir(sub, comps)
					<-s.sem
					mu.Lock()
					asyncSum ^= v
					mu.Unlock()
				}()
			default:
				sum ^= s.scanDir(sub, child)
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
		sum ^= hashEntry(child, info)
	}

	wg.Wait()
	return sum ^ asyncSum
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

func (m *Manager) snapshotTree() (*object.Tree, error) {
	prevHead, err := m.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	m.worktree.Excludes = m.excludes()

	if err := m.worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return nil, fmt.Errorf("failed to stage: %w", err)
	}

	snapshotHash, err := m.worktree.Commit("__live__", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "wingman",
			Email: "wingman@local",
			When:  time.Now(),
		},
		AllowEmptyCommits: true,
	})

	if rollbackErr := m.repo.Storer.SetReference(plumbing.NewHashReference(prevHead.Name(), prevHead.Hash())); rollbackErr != nil {
		if err == nil {
			err = fmt.Errorf("failed to reset HEAD after snapshot: %w", rollbackErr)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("failed to snapshot: %w", err)
	}

	snapshotCommit, err := m.repo.CommitObject(snapshotHash)
	if err != nil {
		return nil, fmt.Errorf("failed to load snapshot commit: %w", err)
	}

	tree, err := snapshotCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshot tree: %w", err)
	}

	return tree, nil
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

	baselineTree, err := baselineCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get baseline tree: %w", err)
	}

	liveTree, err := m.snapshotTree()
	if err != nil {
		return nil, fmt.Errorf("failed to snapshot working tree: %w", err)
	}

	changes, err := baselineTree.Diff(liveTree)
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
