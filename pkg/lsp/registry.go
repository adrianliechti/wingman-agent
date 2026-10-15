package lsp

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Registry is an immutable language-server catalog shared by detection and
// installation requirements. Commands are still resolved only by ManagedTools.
// Loading a catalog never installs software or executes a project/PATH binary.
type Registry struct{ projects []ProjectDefinition }

//go:embed servers.json
var builtinServers []byte

func loadBuiltinProjects() []ProjectDefinition {
	registry, err := parseRegistry(builtinServers, nil)
	if err != nil {
		panic(err)
	}
	return registry.projects
}

// ParseRegistry extends the built-in catalog with declarative project types.
// Overrides are rejected so additions cannot change managed defaults silently.
func ParseRegistry(data []byte) (*Registry, error) {
	return parseRegistry(data, knownProjects)
}

func parseRegistry(data []byte, defaults []ProjectDefinition) (*Registry, error) {
	var additions []ProjectDefinition
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&additions); err != nil {
		return nil, fmt.Errorf("LSP registry: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("LSP registry must contain one JSON array")
	}
	if additions == nil {
		return nil, fmt.Errorf("LSP registry must be an array")
	}
	projects := append([]ProjectDefinition(nil), defaults...)
	seen := make(map[string]bool)
	for _, project := range projects {
		seen[project.Name] = true
	}
	for _, project := range additions {
		if strings.TrimSpace(project.Name) == "" || seen[project.Name] {
			return nil, fmt.Errorf("missing or duplicate LSP project name %q", project.Name)
		}
		seen[project.Name] = true
		if len(project.Markers) == 0 || len(project.Servers) == 0 {
			return nil, fmt.Errorf("%s requires project markers and servers", project.Name)
		}
		for _, pattern := range append(append([]string(nil), project.Markers...), project.Excludes...) {
			if pattern == "" || strings.ContainsAny(pattern, `/\`) {
				return nil, fmt.Errorf("%s: markers/excludes must be filename patterns", project.Name)
			}
			if _, err := filepath.Match(pattern, ""); err != nil {
				return nil, fmt.Errorf("%s: invalid marker pattern: %w", project.Name, err)
			}
		}
		for _, server := range project.Servers {
			if server.Name == "" || server.Command == "" || strings.ContainsAny(server.Command, "/\\ \t\n\r") || server.Command == "." || server.Command == ".." {
				return nil, fmt.Errorf("%s: server name and managed command name required (no paths)", project.Name)
			}
			if len(server.Languages) == 0 || server.LanguageID == "" {
				return nil, fmt.Errorf("%s: languages and language_id required", project.Name)
			}
			if server.MinimumMajorVersion < -1 {
				return nil, fmt.Errorf("%s: invalid minimum_major_version", project.Name)
			}
			for _, extension := range server.Languages {
				if extension == "" || strings.ContainsAny(extension, ". /\\\t\n") {
					return nil, fmt.Errorf("%s: language extensions must omit the dot", project.Name)
				}
			}
			for extension, language := range server.ExtensionToLanguage {
				if !strings.HasPrefix(extension, ".") || language == "" {
					return nil, fmt.Errorf("%s: extension_to_language needs dotted extensions and language IDs", project.Name)
				}
			}
		}
		projects = append(projects, project)
	}
	return &Registry{projects: projects}, nil
}

func registryProjects(registries []*Registry) []ProjectDefinition {
	if len(registries) > 0 && registries[0] != nil {
		return registries[0].projects
	}
	return knownProjects
}

// DetectRequirements uses exactly the catalog passed to WithRegistry.
func (r *Registry) DetectRequirements(root string) []ServerRequirement {
	return detectRequirements(root, r)
}

func WithRegistry(registry *Registry) ManagerOption {
	return func(manager *Manager) { manager.registry = registry }
}
