package lsp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

type managerTestConn struct {
	jsonrpc2.Conn
	closed chan struct{}
}

func (c *managerTestConn) Close() error {
	close(c.closed)
	return nil
}

func newManagerTestSession(project Project) *Session {
	session := &Session{
		server:     project.Server,
		rootURI:    fileuri.FromPath(project.Dir),
		rpc:        protocol.UnimplementedServer{},
		conn:       &managerTestConn{closed: make(chan struct{})},
		cancelFunc: func() {},
	}
	session.alive.Store(true)
	return session
}

func TestWarmupAndRequestsShareSessionStartup(t *testing.T) {
	for _, warmup := range []string{"file", "workspace"} {
		t.Run(warmup, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				project := Project{Dir: root, Server: Server{Name: "test", Languages: []string{"go"}}}
				manager := NewManager(root)
				defer manager.Close()
				manager.roots = []projectRoot{project}
				manager.detectedAt = time.Now()
				path := filepath.Join(root, "main.go")
				want := newManagerTestSession(project)
				release := make(chan struct{})
				var starts atomic.Int32
				manager.connect = func(ctx context.Context, _ string, _ Server) (*Session, error) {
					starts.Add(1)
					select {
					case <-release:
						return want, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}

				if warmup == "file" {
					manager.WarmUpFile(path)
				} else {
					manager.WarmUpServers()
				}
				synctest.Wait()
				if starts.Load() != 1 {
					t.Fatalf("warmup starts = %d, want 1", starts.Load())
				}
				if session, ok := manager.ActiveSession(path); ok || session != nil {
					t.Fatal("pending startup reported an active session")
				}
				if activities := manager.Activities(); len(activities) != 0 {
					t.Fatalf("pending startup activities = %v", activities)
				}

				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if _, err := manager.GetSession(ctx, path); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled request = %v", err)
				}
				results := make(chan *Session, 8)
				for range cap(results) {
					manager.WarmUpFile(path)
					manager.WarmUpServers()
					go func() {
						session, err := manager.GetSession(t.Context(), path)
						if err != nil {
							t.Errorf("GetSession: %v", err)
						}
						results <- session
					}()
				}
				synctest.Wait()
				close(release)
				for range cap(results) {
					if got := <-results; got != want {
						t.Fatalf("GetSession = %p, want shared session %p", got, want)
					}
				}
				manager.WarmUpFile(path)
				manager.WarmUpServers()
				synctest.Wait()
				if starts.Load() != 1 {
					t.Fatalf("shared startup connected %d times, want 1", starts.Load())
				}
				if got, ok := manager.ActiveSession(path); !ok || got != want {
					t.Fatalf("ActiveSession = %p, %v; want %p, true", got, ok, want)
				}
			})
		})
	}
}

func TestFailedSessionStartupCanBeRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		project := Project{Dir: t.TempDir(), Server: Server{Name: "test"}}
		manager := NewManager(project.Dir)
		defer manager.Close()
		want := newManagerTestSession(project)
		failure := errors.New("startup failed")
		release := make(chan struct{})
		var starts atomic.Int32
		manager.connect = func(context.Context, string, Server) (*Session, error) {
			if starts.Add(1) == 1 {
				<-release
				return nil, failure
			}
			return want, nil
		}
		results := make(chan error, 2)
		for range cap(results) {
			go func() {
				_, err := manager.ProjectSession(t.Context(), project)
				results <- err
			}()
		}
		synctest.Wait()
		close(release)
		for range cap(results) {
			if err := <-results; !errors.Is(err, failure) {
				t.Fatalf("shared startup error = %v, want %v", err, failure)
			}
		}
		if got, err := manager.ProjectSession(t.Context(), project); got != want || err != nil {
			t.Fatalf("retry = %p, %v; want %p, nil", got, err, want)
		}
		if starts.Load() != 2 {
			t.Fatalf("startup attempts = %d, want 2", starts.Load())
		}
	})
}

func TestSessionStartupDiscardsStaleConnection(t *testing.T) {
	for _, reason := range []string{"manager is closed", "initialization options changed"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				project := Project{Dir: t.TempDir(), Server: Server{Name: "test"}}
				manager := NewManager(project.Dir)
				defer manager.Close()
				session := newManagerTestSession(project)
				release := make(chan struct{})
				manager.connect = func(context.Context, string, Server) (*Session, error) {
					<-release
					return session, nil
				}
				result := make(chan error, 1)
				go func() {
					_, err := manager.ProjectSession(t.Context(), project)
					result <- err
				}()
				synctest.Wait()
				if reason == "manager is closed" {
					manager.Close()
				} else if err := manager.SetServerInitializationOptions("test", map[string]bool{"enabled": true}); err != nil {
					t.Fatal(err)
				}
				close(release)
				if err := <-result; err == nil || !strings.Contains(err.Error(), reason) {
					t.Fatalf("startup error = %v, want %q", err, reason)
				}
				synctest.Wait()
				if activities := manager.Activities(); len(activities) != 0 {
					t.Fatalf("stale connection published activities: %v", activities)
				}
				select {
				case <-session.conn.(*managerTestConn).closed:
				default:
					t.Fatal("stale connection was not closed")
				}
			})
		})
	}
}
