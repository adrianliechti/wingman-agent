package lsp

import (
	"path/filepath"
	"strings"
)

type Server struct {
	Name                  string            `json:"name"`
	Label                 string            `json:"label,omitempty"`
	Command               string            `json:"command"`
	Args                  []string          `json:"args,omitempty"`
	Languages             []string          `json:"languages"`
	LanguageID            string            `json:"language_id"`
	ExtensionToLanguage   map[string]string `json:"extension_to_language,omitempty"`
	MinimumMajorVersion   int               `json:"minimum_major_version,omitempty"`
	InitializationOptions []byte            `json:"-"`
}

func (s Server) LanguageIDForPath(path string) string {
	if id := s.ExtensionToLanguage[filepath.Ext(path)]; id != "" {
		return id
	}
	if s.LanguageID == "cpp" && filepath.Ext(path) == ".c" {
		return "c"
	}
	if s.LanguageID != "typescript" {
		return s.LanguageID
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".tsx":
		return "typescriptreact"
	default:
		return "typescript"
	}
}

type ProjectDefinition struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Markers  []string `json:"markers"`
	Servers  []Server `json:"servers"`
	Excludes []string `json:"excludes,omitempty"`
}

type projectType = ProjectDefinition

var knownProjects = loadBuiltinProjects()
