package server

import (
	"cmp"
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/adrianliechti/wingman-agent/server/ahp"
	"github.com/google/uuid"
)

// DefaultHostPort is the preferred port of the standalone protocol host.
const DefaultHostPort = 9100

type HostOptions struct {
	// Roots are the directories projects may be opened in: the home directory when empty.
	Roots []string
	// Token must accompany every connection when set.
	Token string
	// Idle is how long a project stays open without clients or work: 15 minutes when zero.
	Idle time.Duration

	disableManagedTools bool
}

// Host serves AHP for any project, opening one when a session names its directory.
type Host struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	id                  string // this host process, which a reconnecting window recognizes
	disableManagedTools bool
	token               string
	handler             http.Handler
	background          sync.WaitGroup
	closeOnce           sync.Once

	mu          sync.Mutex
	creating    map[string]chan struct{} // sessions being created, closed when done
	seq         int64
	clients     map[*connection]struct{}
	sessions    map[string]*Session
	chats       map[string]*Chat
	aliases     map[string]sessionAlias // sessions by URI
	chatAliases map[string]sessionAlias // forked chats by chat URI
	versions    map[string]clientVersion
	web         *Project // the project the host's web UI shows; it stays open and hears of its files
	projects    map[string]*Project
	open        func(dir string) (*Project, error)
	projectMu   sync.Mutex      // serializes project startup and teardown, outside mu
	terminalMu  sync.Mutex      // orders terminal creation: a name is claimed in every project at once
	roots       []string        // directories projects may be opened in; replaced, never changed in place
	picked      map[string]bool // folders the user chose to open, which windows may show
	listed      map[string]bool // folders where an agent keeps sessions, which windows may show

	everywhereMu sync.Mutex
	everywhere   map[string]everywhereListing // agents' sessions of every folder, by provider
}

func NewHost(ctx context.Context, opts *HostOptions) (*Host, error) {
	if opts == nil {
		opts = new(HostOptions)
	}
	roots := opts.Roots
	if len(roots) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		roots = []string{home}
	}
	for _, root := range roots {
		if info, err := os.Stat(root); err != nil {
			return nil, err
		} else if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", root)
		}
	}
	h := newHost(ctx)
	h.token = opts.Token
	h.disableManagedTools = opts.disableManagedTools
	for _, root := range roots {
		h.roots = append(h.roots, canonicalPath(root))
	}
	h.open = func(dir string) (*Project, error) {
		if info, err := os.Stat(dir); err != nil {
			return nil, err
		} else if !info.IsDir() {
			return nil, ahp.InvalidParams("working directory is not a directory")
		}
		return h.newProject(dir)
	}
	h.loadSavedSessions()

	idle := cmp.Or(opts.Idle, 15*time.Minute)
	h.background.Go(func() {
		ticker := time.NewTicker(min(idle, time.Minute))
		defer ticker.Stop()
		for {
			select {
			case <-h.ctx.Done():
				return
			case <-ticker.C:
				h.closeIdle(idle)
			}
		}
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ahp", h.handleAHP)
	h.handler = LocalHostOnly(mux)
	return h, nil
}

// newHost initializes ownership without starting projects, timers or listeners.
func newHost(ctx context.Context) *Host {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Host{
		ctx: ctx, cancel: cancel,
		id:          uuid.NewString(),
		creating:    map[string]chan struct{}{},
		clients:     map[*connection]struct{}{},
		sessions:    map[string]*Session{},
		chats:       map[string]*Chat{},
		aliases:     map[string]sessionAlias{},
		chatAliases: map[string]sessionAlias{},
		versions:    map[string]clientVersion{},
		projects:    map[string]*Project{},
	}
}

func (h *Host) handleAHP(w http.ResponseWriter, r *http.Request) {
	if h.token != "" && subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("tkn")), []byte(h.token)) != 1 {
		http.Error(w, "connection token required", http.StatusUnauthorized)
		return
	}
	h.serve(w, r)
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

func (h *Host) Run(ctx context.Context, port int) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer h.Close()

	listeners, err := listenLocal(port, DefaultHostPort)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "AHP endpoint: ws://localhost:%d/ahp\n", listeners[0].Addr().(*net.TCPAddr).Port)
	return serveLocal(h, listeners, ctx, h.ctx)
}

func (h *Host) Close() {
	h.closeOnce.Do(func() {
		h.cancel()
		h.mu.Lock()
		for _, p := range h.projects {
			p.stop()
		}
		for c := range h.clients {
			c.writer.close()
			_ = c.writer.conn.CloseNow()
		}
		h.mu.Unlock()
		// Join teardown and startup, then close without the lock their operations may wait for.
		h.background.Wait()
		h.projectMu.Lock()
		projects := h.loaded()
		h.projectMu.Unlock()
		for _, p := range projects {
			p.close()
		}
	})
}
