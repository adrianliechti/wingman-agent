package server

import (
	"net"
	"net/http"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/remote"
)

// LocalHostOnly guards local HTTP entry points while permitting authenticated
// relay transports. Same-origin checks do not prevent DNS rebinding: the browser's origin can
// still match a hostile Host after its DNS starts resolving to loopback. The
// unauthenticated local transport only serves literal loopback hosts.
func LocalHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !remote.AuthenticatedTransport(r.Context()) && !isLoopbackHost(r.Host) {
			http.Error(w, "unrecognized workspace host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if value, _, err := net.SplitHostPort(hostport); err == nil {
		host = value
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
