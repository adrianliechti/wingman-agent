package server

import (
	"net/http"
	"strings"
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
		next.ServeHTTP(w, r)
	})
}
