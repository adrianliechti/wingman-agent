package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/filelock"
	"github.com/adrianliechti/wingman-agent/internal/statefile"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
	"github.com/adrianliechti/wingman-agent/server"
)

// Each launch of the desktop app is a process of its own. The first one that
// opens a folder starts a host in the background, from the same executable,
// and every window serves its page from that host. So all windows share one
// session list, and closing a window stops no agent. The host stops once no
// window used it for a while and no agent works.

const (
	hostCommand     = "--background-host"
	hostTokenHeader = "X-Wingman-Host-Token"
)

// hostTiming is how long the host waits before it stops.
type hostTiming struct {
	grace time.Duration // unused, before it stops by itself
	quiet time.Duration // unused, before a window of another build may stop it
}

var defaultTiming = hostTiming{grace: time.Minute, quiet: 10 * time.Second}

// selfVersion identifies the build this process runs. It is taken at start,
// before an update can replace the executable.
var selfVersion = buildVersion()

// hostInfo is the state file through which windows find the running host.
type hostInfo struct {
	PID   int    `json:"pid"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

func hostStatePath() (string, error) { return layout.WingmanPath("app", "host.json") }

func readHostInfo() (hostInfo, error) {
	path, err := hostStatePath()
	if err != nil {
		return hostInfo{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return hostInfo{}, err
	}
	var info hostInfo
	if err := json.Unmarshal(data, &info); err != nil || info.URL == "" || info.Token == "" {
		return hostInfo{}, errors.New("invalid host state")
	}
	return info, nil
}

func writeHostInfo(info hostInfo) error {
	path, err := hostStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return statefile.Write(path, data)
}

// removeHostInfo removes the state file while it still names this host.
func removeHostInfo(info hostInfo) {
	if current, err := readHostInfo(); err == nil && current.Token == info.Token {
		if path, err := hostStatePath(); err == nil {
			_ = os.Remove(path)
		}
	}
}

// buildVersion identifies the executable, so that a window does not use the
// host of another build, such as one left running before an update.
func buildVersion() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	info, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size())
}

func randomToken() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return hex.EncodeToString(secret), nil
}

// runHost is the background host process: it serves until unused or signalled.
func runHost(args []string) error {
	project := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--project" {
			project = args[i+1]
		}
	}
	if project == "" {
		return errors.New("--project is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveHost(ctx, project, nil, defaultTiming)
}

// serveHost runs the shared host with project as its first one. It announces
// itself in the state file once it serves, and ready, when set, hears of it too.
func serveHost(ctx context.Context, project string, ready func(hostInfo), timing hostTiming) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The desktop app's windows open any folder and list every project's sessions.
	srv, err := server.New(ctx, project, &server.ServerOptions{NoBrowser: true, Projects: true})
	if err != nil {
		return err
	}
	defer srv.Close()
	token, err := randomToken()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	handler := &hostHandler{srv: srv, token: token, version: selfVersion, stop: cancel, quiet: timing.quiet}
	handler.touch()
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	info := hostInfo{PID: os.Getpid(), URL: "http://" + listener.Addr().String(), Token: token}
	if err := writeHostInfo(info); err != nil {
		_ = httpServer.Close()
		return err
	}
	defer func() {
		// A replacing window holds the lock until the old listener stops.
		_ = httpServer.Close()
		if unlock, err := lockHostState(true); err == nil {
			defer unlock()
			removeHostInfo(info)
		}
	}()
	if ready != nil {
		ready(info)
	}

	ticker := time.NewTicker(max(min(timing.grace/4, 5*time.Second), 10*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if !srv.Idle() {
			handler.touch()
		} else if handler.unused() >= timing.grace && handler.retire(timing.grace, info) {
			return nil
		}
	}
}

// hostHandler serves the shared host to the windows that know its token.
type hostHandler struct {
	srv     *server.Server
	token   string
	version string
	stop    func()
	quiet   time.Duration
	used    atomic.Int64 // when a window last used the host, in Unix nanoseconds
}

func (h *hostHandler) touch() { h.used.Store(time.Now().UnixNano()) }

func (h *hostHandler) unused() time.Duration { return time.Since(time.Unix(0, h.used.Load())) }

// retire withdraws an unused host from the windows before it stops. It does
// not wait for the state lock: a window that holds it is about to use the host.
func (h *hostHandler) retire(grace time.Duration, info hostInfo) bool {
	unlock, err := lockHostState(false)
	if err != nil {
		return false
	}
	defer unlock()
	if h.unused() < grace || !h.srv.Idle() {
		return false
	}
	removeHostInfo(info)
	return true
}

func (h *hostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(hostTokenHeader)), []byte(h.token)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/__wingman/host":
		writeJSON(w, map[string]any{"version": h.version})
		return
	case "/__wingman/stop":
		// A host of another build makes way for a window's own, unless it is in use.
		if r.Method != http.MethodPost || !h.srv.Idle() || h.unused() < h.quiet {
			http.Error(w, "the host is in use", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		_ = http.NewResponseController(w).Flush()
		h.stop()
		return
	}
	h.touch()
	r.Header.Del(hostTokenHeader)
	h.srv.ServeHTTP(w, r)
}

// hostClient is a window's connection to the shared host: it forwards the
// window's page, API and WebSocket requests and opens folders there.
type hostClient struct {
	info  hostInfo
	proxy *httputil.ReverseProxy
}

func newHostClient(info hostInfo, failed func(error)) (*hostClient, error) {
	target, err := url.Parse(info.URL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		// The request keeps the window's Host: the host checks the window's origin.
		director(r)
		r.Header.Set(hostTokenHeader, info.Token)
	}
	// Streamed answers, such as a workspace search, reach the page at once.
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if failed != nil && r.Context().Err() == nil {
			failed(err)
		}
		http.Error(w, "Wingman host unavailable; reconnecting", http.StatusBadGateway)
	}
	return &hostClient{info: info, proxy: proxy}, nil
}

func (c *hostClient) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Control requests originate in the launcher, outside the browser proxy.
	if strings.HasPrefix(r.URL.Path, "/__wingman/") {
		http.NotFound(w, r)
		return
	}
	c.proxy.ServeHTTP(w, r)
}

// call makes a control request to the host and returns its answer.
func (c *hostClient) call(method, path string, body any, timeout time.Duration) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.info.URL, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set(hostTokenHeader, c.info.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	return response.StatusCode, data, err
}

// version asks the host which build it runs; an error means it does not answer.
func (c *hostClient) version() (string, error) {
	status, data, err := c.call(http.MethodGet, "/__wingman/host", nil, 5*time.Second)
	if err != nil {
		return "", err
	}
	var answer struct {
		Version string `json:"version"`
	}
	if status != http.StatusOK || json.Unmarshal(data, &answer) != nil {
		return "", fmt.Errorf("host answered %d", status)
	}
	return answer.Version, nil
}

// serving reports whether the host still answers and is the one windows find.
func (c *hostClient) serving() bool {
	if info, err := readHostInfo(); err != nil || info.Token != c.info.Token {
		return false
	}
	_, err := c.version()
	return err == nil
}

// open opens a folder on the host as the page of its window does, so that the
// window finds its project there.
func (c *hostClient) open(dir string) error {
	status, data, err := c.call(http.MethodGet, "/api/workspace?project="+url.QueryEscape(dir), nil, 2*time.Minute)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return errors.New(strings.TrimSpace(string(data)))
	}
	return nil
}

// stop asks an unused host to stop; a host in use refuses.
func (c *hostClient) stop() error {
	status, _, err := c.call(http.MethodPost, "/__wingman/stop", nil, 10*time.Second)
	if err != nil {
		// Older hosts can close their listener before the stop reply arrives.
		if !c.serving() {
			return nil
		}
		return err
	}
	if status != http.StatusNoContent {
		return errors.New("the host is in use")
	}
	return nil
}

// spawnHost starts the background host process with dir as its first project
// and waits until it announces itself.
func spawnHost(dir string) (hostInfo, error) {
	exe, err := os.Executable()
	if err != nil {
		return hostInfo{}, err
	}
	cmd := exec.Command(exe, hostCommand, "--project", dir)
	detach(cmd)
	if path, err := layout.WingmanPath("app", "host.log"); err == nil {
		if logFile, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
			defer logFile.Close()
			cmd.Stdout, cmd.Stderr = logFile, logFile
		}
	}
	if err := cmd.Start(); err != nil {
		return hostInfo{}, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(time.Minute)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-exited:
			return hostInfo{}, fmt.Errorf("the Wingman host stopped while starting: %v", err)
		case <-deadline:
			_ = cmd.Process.Kill()
			return hostInfo{}, errors.New("the Wingman host did not start in time")
		case <-ticker.C:
			if info, err := readHostInfo(); err == nil && info.PID == cmd.Process.Pid {
				return info, nil
			}
		}
	}
}

// attach opens dir on the shared host, which the window then uses, after
// finding or starting the host if the window has none yet or its host
// stopped. It holds the state lock throughout, so that the host cannot retire
// in between.
func (a *App) attach(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("project must be an absolute directory")
	}
	unlock, err := lockHostState(true)
	if err != nil {
		return err
	}
	defer unlock()
	client := a.host.Load()
	if client == nil || !client.serving() {
		if client, err = a.connect(dir); err != nil {
			return err
		}
	}
	if err := client.open(dir); err != nil {
		return err
	}
	a.host.Store(client)
	return nil
}

// lockHostState takes the lock under which windows find or start the shared
// host and the host retires. Without wait, it fails while someone holds it.
// The system releases it when its process dies.
func lockHostState(wait bool) (func(), error) {
	path, err := layout.WingmanPath("app", "host.lock")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	unlock, err := filelock.Lock(file, wait)
	if err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		unlock()
		file.Close()
	}, nil
}

// connect finds the running host, or starts one with dir as its first project.
// The caller holds the state lock.
func (a *App) connect(dir string) (*hostClient, error) {
	if info, err := readHostInfo(); err == nil {
		client, err := newHostClient(info, a.lost)
		if err != nil {
			return nil, err
		}
		if version, err := client.version(); err == nil {
			if version == selfVersion {
				return client, nil
			}
			// A host of another build keeps serving the windows that use it.
			if err := client.stop(); err != nil {
				log.Printf("using the running host of another build: %v", err)
				return client, nil
			}
			waitHostGone(client)
		}
	}
	info, err := a.start(dir)
	if err != nil {
		return nil, err
	}
	return newHostClient(info, a.lost)
}

// waitHostGone waits briefly for a stopping host to stop answering.
func waitHostGone(client *hostClient) {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err := client.version(); err != nil {
			return
		}
	}
}

// lost reconnects the window after its host stopped answering: it finds or
// starts a host again, and the page reloads its project there.
func (a *App) lost(error) {
	if !a.reconnecting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.reconnecting.Store(false)
		a.openMu.Lock()
		defer a.openMu.Unlock()
		current := a.host.Load()
		if current == nil || current.serving() {
			return // a single failed request, not a stopped host
		}
		for _, dir := range a.projects {
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				continue
			}
			if err := a.attach(dir); err != nil {
				log.Printf("reconnect to the Wingman host: %v", err)
			}
			return
		}
	}()
}
