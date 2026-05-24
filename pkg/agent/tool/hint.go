package tool

import (
	"encoding/json"
	"strings"
)

// fsTools' path args are workspace-relative; we render them with a leading "/" so they're
// visually distinct as a workspace path rather than a loose identifier.
var fsTools = map[string]bool{
	"read": true, "write": true, "edit": true,
	"grep": true, "glob": true,
}

// workingDirTools default to the workspace root when their path arg is empty or ".".
var workingDirTools = map[string]bool{
	"grep": true, "glob": true,
}

// ExtractHint returns a short, human-readable summary of a tool call's input,
// suitable as a one-line UI label (e.g. "edit: /foo.go", "grep: pattern",
// "shell: build description"). The result is the bare value — callers usually
// prefix it with the tool name. Used by every UI surface (TUI, web UI live
// stream, web UI replay) so the same call renders identically everywhere.
func ExtractHint(argsJSON, toolName string) string {
	var args map[string]any

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return wdFallback(toolName)
	}

	if desc, ok := args["description"]; ok {
		if str, ok := desc.(string); ok && str != "" {
			return strings.Join(strings.Fields(str), " ")
		}
	}

	hintKeys := []string{
		"query",
		"pattern",
		"command",
		"prompt",
		"question",
		"file_path",
		"path",
		"file",
		"url",
		"name",
	}

	for _, key := range hintKeys {
		val, ok := args[key]
		if !ok {
			continue
		}
		str, ok := val.(string)
		if !ok || str == "" {
			continue
		}
		normalized := strings.Join(strings.Fields(str), " ")
		if (key == "file_path" || key == "path" || key == "file") && fsTools[toolName] {
			normalized = normalizeWorkspacePath(normalized)
		}
		return normalized
	}

	return wdFallback(toolName)
}

// normalizeWorkspacePath rewrites a workspace-relative path so that it always starts with "/".
// The cwd literals "." and "./" become "/". Already-absolute paths pass through unchanged.
func normalizeWorkspacePath(p string) string {
	if p == "" || p == "." || p == "./" {
		return "/"
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return p
	}
	return "/" + p
}

func wdFallback(toolName string) string {
	if workingDirTools[toolName] {
		return "/"
	}
	return ""
}
