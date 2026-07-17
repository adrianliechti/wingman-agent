package code

import (
	"os"
	"path/filepath"
	"strings"
)

func detectFilePaths(text, workingDir string) []string {
	lines := strings.Split(strings.TrimSpace(text), "\n")

	var paths []string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if len(line) >= 2 {
			if (line[0] == '"' && line[len(line)-1] == '"') || (line[0] == '\'' && line[len(line)-1] == '\'') {
				line = line[1 : len(line)-1]
			}
		}

		if line == "" {
			continue
		}

		if !isLikelyFilePath(line) {
			continue
		}

		resolved := resolveFilePath(line, workingDir)
		if resolved == "" {
			continue
		}

		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			continue
		}

		paths = append(paths, resolved)
	}

	return paths
}

func isLikelyFilePath(s string) bool {
	if strings.ContainsAny(s, "{}<>|") {
		return false
	}

	if !strings.Contains(s, "/") && !strings.Contains(s, "\\") {
		return false
	}

	if filepath.IsAbs(s) {
		return true
	}

	if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		return true
	}

	if strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, `.\`) || strings.HasPrefix(s, `..\`) {
		return true
	}

	return false
}

func resolveFilePath(path, workingDir string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}

		path = filepath.Join(home, path[2:])
	}

	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDir, path)
	}

	return filepath.Clean(path)
}

func normalizeFilePath(absPath, workingDir string) string {
	rel, err := filepath.Rel(workingDir, absPath)
	if err != nil {
		return absPath
	}

	if strings.HasPrefix(rel, "..") {
		return absPath
	}

	return rel
}
