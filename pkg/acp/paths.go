package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// NormalizeSessionRoots validates ACP's absolute-path contract and returns a
// cleaned, de-duplicated set of additional roots that excludes cwd itself.
func NormalizeSessionRoots(cwd string, additionalDirectories []string) (string, []string, error) {
	if cwd == "" {
		return "", nil, acp.NewInvalidParams("cwd is required")
	}
	if strings.ContainsRune(cwd, '\x00') {
		return "", nil, acp.NewInvalidParams("cwd must not contain NUL bytes")
	}
	if !filepath.IsAbs(cwd) {
		return "", nil, acp.NewInvalidParams(fmt.Sprintf("cwd must be an absolute path (got %q)", cwd))
	}
	cwd = filepath.Clean(cwd)
	seen := map[string]bool{cwd: true}
	out := make([]string, 0, len(additionalDirectories))
	for _, directory := range additionalDirectories {
		if directory == "" {
			return "", nil, acp.NewInvalidParams("additionalDirectories entries must not be empty")
		}
		if strings.ContainsRune(directory, '\x00') {
			return "", nil, acp.NewInvalidParams("additionalDirectories entries must not contain NUL bytes")
		}
		if !filepath.IsAbs(directory) {
			return "", nil, acp.NewInvalidParams(fmt.Sprintf("additionalDirectories entries must be absolute (got %q)", directory))
		}
		directory = filepath.Clean(directory)
		if !seen[directory] {
			seen[directory] = true
			out = append(out, directory)
		}
	}
	return cwd, out, nil
}

// ResolvePath resolves a tool-reported path against the session cwd. A path
// that is rooted but has no volume (\dir\file on Windows) keeps cwd's volume
// instead of being nested beneath cwd.
func ResolvePath(path, cwd string) string {
	if path == "" || cwd == "" || filepath.IsAbs(path) {
		return path
	}
	if os.IsPathSeparator(path[0]) {
		return filepath.VolumeName(cwd) + path
	}
	return filepath.Join(cwd, path)
}

// SessionNotFound identifies a missing or inactive session on the wire.
func SessionNotFound(id acp.SessionId) *acp.RequestError {
	return acp.NewRequestError(-32002, "Resource not found", map[string]any{"sessionId": id})
}
