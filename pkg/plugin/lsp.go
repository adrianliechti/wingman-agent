package plugin

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/external"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

// Claude Code's plugin files for language servers.
const (
	languageServersFile  = ".lsp.json"
	languageServersField = "lspServers"
	claudeManifest       = ".claude-plugin/plugin.json"
)

// loadClaudePlugin reads the language servers of a plugin in Claude Code's layout, which needs no manifest.
func loadClaudePlugin(dir, dataRoot string) (*Plugin, []string) {
	root, err := resolvePath(dir)
	if err != nil {
		return nil, nil
	}
	var manifest struct {
		Name            string          `json:"name"`
		LanguageServers json.RawMessage `json:"lspServers"`
	}
	var notes []string
	if data, err := readPluginFile(root, claudeManifest); err == nil {
		if err := json.Unmarshal(data, &manifest); err != nil {
			notes = append(notes, fmt.Sprintf("ignoring %s: %v", claudeManifest, err))
		}
	} else if !os.IsNotExist(err) {
		notes = append(notes, fmt.Sprintf("ignoring %s: %v", claudeManifest, err))
	}
	name := manifest.Name
	if name == "" {
		name = filepath.Base(root)
	}
	p := &Plugin{Name: name, Root: root, Manifest: Manifest{Name: name}, Hooks: &external.Config{}}
	servers, serverNotes := loadLanguageServers(p, manifest.LanguageServers)
	notes = append(notes, serverNotes...)
	if len(servers) == 0 {
		return nil, notes
	}
	if err := validateName(name); err != nil {
		return nil, append(notes, err.Error())
	}
	if p.Data, err = dataDirectory(dataRoot, name); err != nil {
		return nil, append(notes, err.Error())
	}
	p.LanguageServers = servers
	return p, notes
}

// lspServers is a path, an inline map of servers, or a list of either; later names replace earlier ones.
func loadLanguageServers(p *Plugin, manifest json.RawMessage) ([]lsp.Server, []string) {
	var notes []string
	var servers []lsp.Server
	add := func(source string, data []byte) {
		parsed, err := lsp.ParseServers(data)
		if err != nil {
			notes = append(notes, fmt.Sprintf("ignoring %s: %v", source, err))
			return
		}
		for _, server := range parsed {
			servers = lsp.ReplaceServer(servers, server)
		}
	}
	addFile := func(path string, optional bool) {
		data, err := readPluginFile(p.Root, path)
		if err != nil {
			if !optional || !os.IsNotExist(err) {
				notes = append(notes, fmt.Sprintf("ignoring language servers in %s: %v", path, err))
			}
			return
		}
		add(path, data)
	}

	addFile(languageServersFile, true)

	entries := []json.RawMessage{manifest}
	if isArray(manifest) {
		if err := json.Unmarshal(manifest, &entries); err != nil {
			notes = append(notes, fmt.Sprintf("ignoring %s: %v", languageServersField, err))
			entries = nil
		}
	}
	for _, entry := range entries {
		var path string
		switch {
		case len(entry) == 0:
		case json.Unmarshal(entry, &path) == nil:
			addFile(path, false)
		case isObject(entry):
			add(languageServersField, entry)
		default:
			notes = append(notes, fmt.Sprintf("ignoring %s: each entry is a path or an object of servers", languageServersField))
		}
	}
	return servers, notes
}

func readPluginFile(root, path string) ([]byte, error) {
	if filepath.IsAbs(path) {
		return nil, fmt.Errorf("the path must be relative to the plugin")
	}
	file := filepath.Join(root, filepath.FromSlash(path))
	if _, err := os.Lstat(file); err != nil {
		return nil, err
	}
	resolved, err := resolvePath(file)
	if err != nil || !contains(root, resolved) {
		return nil, fmt.Errorf("the path resolves outside the plugin root")
	}
	if info, err := os.Stat(resolved); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	return os.ReadFile(resolved)
}

func isArray(raw json.RawMessage) bool {
	return strings.HasPrefix(strings.TrimSpace(string(raw)), "[")
}

// A project plugin's servers are left out: a checked-out repository must not choose a command to run.
func LanguageServers(plugins []Plugin, projectDir string) ([]lsp.Server, []Diagnostic) {
	var servers []lsp.Server
	var diagnostics []Diagnostic
	names := make(map[string]string)
	extensions := make(map[string]string)

	for _, p := range plugins {
		if len(p.LanguageServers) == 0 {
			continue
		}
		report := func(format string, args ...any) {
			diagnostics = append(diagnostics, Diagnostic{Path: p.Root, Message: fmt.Sprintf(format, args...)})
		}
		if !p.Personal {
			report("language servers of a project plugin are not started; install the plugin in your personal plugins directory to use them")
			continue
		}
		for _, server := range p.LanguageServers {
			if owner, ok := names[server.Name]; ok {
				report("language server %q is shadowed by plugin %q", server.Name, owner)
				continue
			}
			names[server.Name] = p.Name
			for _, extension := range slices.Sorted(maps.Keys(server.ExtensionToLanguage)) {
				if owner, ok := extensions[extension]; ok {
					report("language server %q is not used for %s files; %q handles them", server.Name, extension, owner)
				} else {
					extensions[extension] = server.Name
				}
			}
			servers = append(servers, placeLanguageServer(server, p, projectDir))
		}
	}

	return servers, diagnostics
}

// Claude Code's variable names work beside the portable ones and reach the server's process.
func placeLanguageServer(server lsp.Server, p Plugin, projectDir string) lsp.Server {
	variables := map[string]string{
		rootVariable: p.Root, "CLAUDE_PLUGIN_ROOT": p.Root,
		dataVariable: p.Data, "CLAUDE_PLUGIN_DATA": p.Data,
		"CLAUDE_PROJECT_DIR": projectDir,
	}
	expand := func(value string) string { return external.Expand(value, variables) }

	server.Command = expand(server.Command)
	server.WorkspaceFolder = expand(server.WorkspaceFolder)
	server.Args = slices.Clone(server.Args)
	for i, arg := range server.Args {
		server.Args[i] = expand(arg)
	}
	env := maps.Clone(variables)
	for name, value := range server.Env {
		env[name] = expand(value)
	}
	server.Env = env
	return server
}
