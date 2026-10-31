package terminal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	maxScrollback = 256 << 10
	compactSlack  = 64 << 10

	readChunk = 32 << 10

	DefaultCols = 80
	DefaultRows = 24
)

var ErrUnsupported = errors.New("terminal sessions are not supported on this platform")

// Terminal is a running command attached to a pseudo-terminal. Each platform
// builds one its own way — Unix over creack/pty and an exec.Cmd, Windows over
// ConPTY — but Session only needs this platform-neutral surface.
type Terminal interface {
	// Read and Write move terminal I/O. Read returns io.EOF (or another error)
	// once the process exits and the master side drains.
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)

	// Resize changes the pseudo-terminal window size.
	Resize(cols, rows int) error

	// ProcessID is the child process id, or 0 before it starts.
	ProcessID() int

	// HasForegroundProcess reports whether a command other than the interactive
	// shell currently owns the terminal foreground. Platforms without the notion
	// return false.
	HasForegroundProcess() bool

	// Wait blocks until the process exits.
	Wait() error

	// Close terminates the process and releases the pseudo-terminal.
	Close() error
}

// CommandSpec describes a trusted, direct process launch inside a PTY. It is
// intentionally separate from the public shell endpoint: editor services such
// as DAP use it after the user has reviewed the operation, while browser input
// can still only select from known shells.
type CommandSpec struct {
	Path  string
	Args  []string
	Dir   string
	Env   map[string]*string
	Title string
}

type Session struct {
	id      string
	title   string
	shell   string
	command bool

	pty Terminal

	done   chan struct{}
	onExit func(*Session)

	mu        sync.Mutex
	buf       []byte
	subs      map[*subscriber]struct{}
	cols      int
	rows      int
	closed    bool
	stopOnce  sync.Once
	exitCode  int
	exitKnown bool
	cwd       string
}

func Supported() bool {
	return ptySupported
}

func newSession(id, shell, dir string, cols, rows int, onExit func(*Session)) (*Session, error) {
	return newCommandSession(id, CommandSpec{Path: shell, Dir: dir}, cols, rows, onExit)
}

func newCommandSession(id string, spec CommandSpec, cols, rows int, onExit func(*Session)) (*Session, error) {
	if !ptySupported {
		return nil, ErrUnsupported
	}

	cols, rows = normalizeSize(cols, rows)
	path := strings.TrimSpace(spec.Path)
	if path == "" {
		return nil, errors.New("terminal command is required")
	}
	dir := strings.TrimSpace(spec.Dir)
	if dir == "" {
		dir = "."
	}
	if strings.ContainsRune(path, filepath.Separator) && !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if resolved, err := exec.LookPath(path); err == nil {
		path = resolved
	} else if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("terminal command %q was not found", spec.Path)
	}

	launch := CommandSpec{
		Path:  path,
		Args:  spec.Args,
		Dir:   dir,
		Env:   spec.Env,
		Title: spec.Title,
	}

	p, err := startPTY(launch, cols, rows)
	if err != nil {
		return nil, err
	}

	s := &Session{
		id:    id,
		title: strings.TrimSpace(spec.Title),
		shell: path,
		cwd:   dir,

		pty: p,

		done:   make(chan struct{}),
		onExit: onExit,
		subs:   map[*subscriber]struct{}{},

		cols: cols,
		rows: rows,
	}
	if s.title == "" {
		s.title = shellName(path)
	}

	go s.read()

	return s, nil
}

// IsCommand reports a session created by CreateCommand rather than a shell.
func (s *Session) IsCommand() bool { return s.command }

func (s *Session) ID() string {
	return s.id
}

func (s *Session) Title() string {
	return s.title
}

func (s *Session) Shell() string {
	return s.shell
}

func (s *Session) Cwd() string { return s.cwd }

// ExitCode is available after Done closes. A killed process may have no code.
func (s *Session) ExitCode() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode, s.exitKnown
}

func (s *Session) ProcessID() int {
	if s.pty == nil {
		return 0
	}
	return s.pty.ProcessID()
}

func (s *Session) Size() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

func (s *Session) Done() <-chan struct{} {
	return s.done
}

func (s *Session) Exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// HasForegroundProcess reports whether the PTY foreground process group is a
// command rather than the interactive shell itself.
func (s *Session) HasForegroundProcess() bool {
	if s.Exited() {
		return false
	}
	return s.pty.HasForegroundProcess()
}

func (s *Session) Write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	_, err := s.pty.Write(p)
	return err
}

func (s *Session) Resize(cols, rows int) error {
	cols, rows = normalizeSize(cols, rows)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || (cols == s.cols && rows == s.rows) {
		return nil
	}
	// Apply and publish sizes in the same order. A failed resize must leave
	// the last applied size intact so the same request can be retried.
	if err := s.pty.Resize(cols, rows); err != nil {
		return err
	}
	s.cols, s.rows = cols, rows
	return nil
}

// Subscribe returns the scrollback captured so far plus a channel of
// subsequent output. Bursts are coalesced; the channel is closed once the
// session's output is delivered after it ends, or when the subscriber falls a
// whole scrollback behind and must subscribe again. cancel must be called.
func (s *Session) Subscribe() (snapshot []byte, ch <-chan []byte, cancel func()) {
	s.mu.Lock()
	snapshot = append([]byte(nil), s.buf...)
	if s.closed {
		s.mu.Unlock()
		c := make(chan []byte)
		close(c)
		return snapshot, c, func() {}
	}
	sub := newSubscriber()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()

	return snapshot, sub.out, func() { s.unsubscribe(sub) }
}

func (s *Session) unsubscribe(sub *subscriber) {
	s.mu.Lock()
	delete(s.subs, sub)
	s.mu.Unlock()
	sub.cancel()
}

type subscriber struct {
	out      chan []byte
	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once

	mu      sync.Mutex
	pending []byte
	ended   bool // deliver what is pending, then close
	behind  bool // close without delivering
}

func newSubscriber() *subscriber {
	sub := &subscriber{out: make(chan []byte), wake: make(chan struct{}, 1), stop: make(chan struct{})}
	go sub.run()
	return sub
}

func (sub *subscriber) add(chunk []byte) {
	sub.mu.Lock()
	if len(sub.pending)+len(chunk) > maxScrollback {
		sub.behind, sub.pending = true, nil
	} else if !sub.behind {
		sub.pending = append(sub.pending, chunk...)
	}
	sub.mu.Unlock()
	sub.signal()
}

func (sub *subscriber) end() {
	sub.mu.Lock()
	sub.ended = true
	sub.mu.Unlock()
	sub.signal()
}

func (sub *subscriber) signal() {
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}

func (sub *subscriber) cancel() { sub.stopOnce.Do(func() { close(sub.stop) }) }

func (sub *subscriber) run() {
	defer close(sub.out)
	for {
		select {
		case <-sub.wake:
		case <-sub.stop:
			return
		}
		for {
			sub.mu.Lock()
			data, ended, behind := sub.pending, sub.ended, sub.behind
			sub.pending = nil
			sub.mu.Unlock()
			if behind || len(data) == 0 && ended {
				return
			}
			if len(data) == 0 {
				break
			}
			select {
			case sub.out <- data:
			case <-sub.stop:
				return
			}
		}
	}
}

func (s *Session) Close() error {
	var err error
	s.stopOnce.Do(func() { err = s.pty.Close() })
	<-s.done
	return err
}

func (s *Session) read() {
	buf := make([]byte, readChunk)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.publish(buf[:n])
		}
		if err != nil {
			break
		}
	}
	s.finish()
}

func (s *Session) publish(p []byte) {
	chunk := append([]byte(nil), p...)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.buf = append(s.buf, chunk...)
	if len(s.buf) > maxScrollback+compactSlack {
		s.buf = append(make([]byte, 0, maxScrollback+compactSlack), SafeTail(s.buf, maxScrollback)...)
	}

	for sub := range s.subs {
		sub.add(chunk)
	}
}

func (s *Session) finish() {
	// Wait before closing the PTY so a natural exit retains its actual status.
	// Close remains able to terminate the process while Wait is in progress.
	err := s.pty.Wait()
	s.stopOnce.Do(func() { _ = s.pty.Close() })
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	var exit interface{ ExitCode() int }
	if err == nil {
		s.exitKnown = true
	} else if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		s.exitCode, s.exitKnown = exit.ExitCode(), true
	}
	subs := s.subs
	s.subs = map[*subscriber]struct{}{}
	s.mu.Unlock()

	close(s.done)
	for sub := range subs {
		sub.end()
	}

	if s.onExit != nil {
		s.onExit(s)
	}
}

// SafeTail returns at most limit trailing bytes of data. It starts after a
// newline or at an escape sequence when one is near, otherwise at a character,
// so a replay never begins inside an escape sequence or a UTF-8 character.
func SafeTail(data []byte, limit int) []byte {
	if len(data) <= limit {
		return data
	}
	tail := data[len(data)-limit:]
	if i := bytes.IndexAny(tail[:min(len(tail), 4<<10)], "\n\x1b"); i >= 0 {
		if tail[i] == '\n' {
			return tail[i+1:]
		}
		return tail[i:]
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail
}

func normalizeSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = DefaultCols
	}
	if rows <= 0 {
		rows = DefaultRows
	}
	return min(cols, 1000), min(rows, 1000)
}

func terminalEnv() []string {
	drop := []string{"TERM=", "COLORTERM=", "TERM_PROGRAM=", "WINGMAN="}
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		skip := false
		for _, p := range drop {
			if strings.HasPrefix(kv, p) {
				skip = true
				break
			}
		}
		if !skip {
			env = append(env, kv)
		}
	}
	return append(env,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"TERM_PROGRAM=wingman",
		"WINGMAN=1",
	)
}

func commandEnv(overrides map[string]*string) []string {
	if len(overrides) == 0 {
		return terminalEnv()
	}
	values := make(map[string]string)
	for _, item := range terminalEnv() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		key = strings.TrimSpace(key)
		if key == "" || strings.Contains(key, "=") {
			continue
		}
		if value == nil {
			delete(values, key)
		} else {
			values[key] = *value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
