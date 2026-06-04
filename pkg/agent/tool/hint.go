package tool

import (
	"encoding/json"
	"strings"
)

var fsTools = map[string]bool{
	"read": true, "write": true, "edit": true,
	"grep": true, "glob": true,
}

var workingDirTools = map[string]bool{
	"grep": true, "glob": true,
}

func ExtractHint(argsJSON, toolName string) string {
	var args map[string]any

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return wdFallback(toolName)
	}

	if desc, ok := args["description"]; ok {
		if str, ok := desc.(string); ok && str != "" {
			label := strings.Join(strings.Fields(str), " ")

			if toolName == "agent" {
				if at, ok := args["agent_type"].(string); ok && at != "" {
					label += " (" + strings.TrimSpace(at) + ")"
				}
			}
			return label
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
