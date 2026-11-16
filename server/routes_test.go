package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatHandlerKeepsBackendAliveUntilRequestReturns(t *testing.T) {
	for _, source := range []string{"request", "workspace"} {
		t.Run(source, func(t *testing.T) {
			s, _, base := newProtocolServer(t)
			var agentClosed atomic.Bool
			b := installProtocolAgent(s, &lifecycleAgent{protocolAgent: base, closeAgent: func() error {
				agentClosed.Store(true)
				return nil
			}})
			chat, err := s.Project.attachSession(b.session("native-session"), "ahp-session:/http", "HTTP", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			s.mux.Get("/read/{chat}", s.chatHandler(func(c *sessionController, w http.ResponseWriter, r *http.Request) {
				if c.ref.SessionID != "native-session" || c.backend != b {
					t.Error("request reached a different native session")
				}
				close(started)
				<-r.Context().Done()
				close(cancelled)
				<-release
				w.WriteHeader(http.StatusNoContent)
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/read/"+url.PathEscape(chat.chat.Resource), nil).WithContext(ctx)
			response := httptest.NewRecorder()
			returned := make(chan struct{})
			go func() { s.ServeHTTP(response, req); close(returned) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("chat request did not reach its handler")
			}
			closed := make(chan struct{})
			if source == "request" {
				cancel()
			} else {
				go func() { s.Close(); close(closed) }()
			}
			select {
			case <-cancelled:
			case <-time.After(5 * time.Second):
				t.Fatal("chat request did not observe cancellation")
			}
			if agentClosed.Load() {
				t.Fatal("backend closed before its request returned")
			}
			unblock()
			<-returned
			if source == "workspace" {
				<-closed
			}
			if response.Code != http.StatusNoContent {
				t.Fatalf("chat request returned %d: %s", response.Code, response.Body)
			}
		})
	}
}
