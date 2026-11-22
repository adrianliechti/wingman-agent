package server

import (
	"net/http"
	"strings"
	"time"
)

const instanceHeader = "X-Wingman-Instance"

func (s *Server) checkInstance(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		instance := r.Header.Get(instanceHeader)
		if instance == "" {
			instance = r.URL.Query().Get("instance")
		}
		// Discovery is initially unscoped. Later refreshes carry the instance
		// and must reject a replacement workspace like all other operations.
		protected := (strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/workspace") || r.URL.Path == "/lsp" || r.URL.Path == "/dap" || r.URL.Path == "/content" || r.URL.Path == "/preview"
		if protected || r.URL.Path == "/api/workspace" && instance != "" {
			if instance != s.Project.scope.InstanceID || s.Project.ctx.Err() != nil {
				http.Error(w, "workspace instance changed; reload the workspace", http.StatusConflict)
				return
			}
		}
		// A temporary chat lasts while a window shows it, even the host's first project.
		if s.front != nil || s.Project.temporary != nil {
			p := s.Project
			p.host.mu.Lock()
			if !p.availableLocked() {
				p.host.mu.Unlock()
				http.Error(w, "workspace instance changed; reload the workspace", http.StatusConflict)
				return
			}
			p.windowRequests++
			p.host.mu.Unlock()
			defer func() {
				p.host.mu.Lock()
				p.windowRequests--
				p.used = time.Now()
				p.host.mu.Unlock()
			}()
		}
		next.ServeHTTP(w, r)
	})
}
