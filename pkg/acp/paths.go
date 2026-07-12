package acp

import (
	"fmt"
	"path/filepath"
)

// NormalizeSessionRoots validates ACP's absolute-path contract and returns a
// cleaned, de-duplicated set of additional roots that excludes cwd itself.
func NormalizeSessionRoots(cwd string, additionalDirectories []string) (string, []string, error) {
	if cwd == "" {
		return "", nil, fmt.Errorf("cwd is required")
	}
	if !filepath.IsAbs(cwd) {
		return "", nil, fmt.Errorf("cwd must be an absolute path (got %q)", cwd)
	}
	cwd = filepath.Clean(cwd)
	seen := map[string]bool{cwd: true}
	out := make([]string, 0, len(additionalDirectories))
	for _, directory := range additionalDirectories {
		if directory == "" {
			return "", nil, fmt.Errorf("additionalDirectories entries must not be empty")
		}
		if !filepath.IsAbs(directory) {
			return "", nil, fmt.Errorf("additionalDirectories entries must be absolute (got %q)", directory)
		}
		directory = filepath.Clean(directory)
		if !seen[directory] {
			seen[directory] = true
			out = append(out, directory)
		}
	}
	return cwd, out, nil
}
