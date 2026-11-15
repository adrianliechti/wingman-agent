package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/adrianliechti/wingman-agent/pkg/layout"
)

type HostOptions struct {
	// Roots are the directories projects may be opened in: the home directory when empty.
	Roots []string
	// Token must accompany every connection when set.
	Token string

	disableManagedTools bool
}

// Host serves AHP for any project, opening one when a session names its directory.
type Host struct {
	ctx       context.Context
	cancel    context.CancelFunc
	ahp       *ahpHost
	token     string
	handler   http.Handler
	closeOnce sync.Once
}

func NewHost(ctx context.Context, opts *HostOptions) (*Host, error) {
	if opts == nil {
		opts = new(HostOptions)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	roots := opts.Roots
	if len(roots) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		roots = []string{home}
	}
	hostCtx, cancel := context.WithCancel(ctx)
	h := &Host{ctx: hostCtx, cancel: cancel, ahp: newAHPHost(), token: opts.Token}
	for _, root := range roots {
		h.ahp.roots = append(h.ahp.roots, canonicalPath(root))
	}
	h.ahp.open = func(dir string) (*Server, error) {
		if info, err := os.Stat(dir); err != nil {
			return nil, err
		} else if !info.IsDir() {
			return nil, ahpInvalid("working directory is not a directory")
		}
		return newServer(hostCtx, dir, &ServerOptions{NoBrowser: true, disableManagedTools: opts.disableManagedTools}, h.ahp)
	}
	h.ahp.loadSavedSessions()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ahp", h.handleAHP)
	h.handler = LocalHostOnly(mux)
	return h, nil
}

func (h *ahpHost) loadSavedSessions() {
	dir, err := layout.WingmanPath("ahp")
	if err != nil {
		return
	}
	projects, _ := os.ReadDir(dir)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		for _, alias := range readAliases(filepath.Join(dir, project.Name())) {
			// Sessions saved before hosts served several projects name none.
			if alias.Project != "" && h.serves(alias.Project) {
				h.rememberAliasLocked(alias)
			}
		}
	}
}

func (h *Host) handleAHP(w http.ResponseWriter, r *http.Request) {
	if h.token != "" && subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("tkn")), []byte(h.token)) != 1 {
		http.Error(w, "connection token required", http.StatusUnauthorized)
		return
	}
	h.ahp.serve(h.ctx, w, r)
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
		// A project still opening closes itself once it finds the host closed.
		h.ahp.close()
		for _, p := range h.ahp.loaded() {
			p.server.Close()
		}
	})
}
