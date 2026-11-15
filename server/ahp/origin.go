package ahp

import (
	"net/http"
	"net/url"
	"strings"
)

// Web pages always send an http(s) or "null" Origin, so only the same-origin
// Web UI may use one. Native clients send none; desktop web views (VS Code's
// vscode-file://) use their own schemes, which no website can claim.
func OriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return strings.EqualFold(u.Host, r.Host)
	}
	return true
}
