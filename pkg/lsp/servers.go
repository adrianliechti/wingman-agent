package lsp

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/tooling"
)

// Server is a language server in Claude Code's lspServers format (.lsp.json).
type Server struct {
	Name                  string            `json:"-"`
	Command               string            `json:"command"`
	Args                  []string          `json:"args,omitempty"`
	ExtensionToLanguage   map[string]string `json:"extensionToLanguage"`
	Transport             string            `json:"transport,omitempty"` // stdio or socket; every server runs over stdio
	Env                   map[string]string `json:"env,omitempty"`
	InitializationOptions json.RawMessage   `json:"initializationOptions,omitempty"`
	Settings              json.RawMessage   `json:"settings,omitempty"` // sent with workspace/didChangeConfiguration
	WorkspaceFolder       string            `json:"workspaceFolder,omitempty"`
	StartupTimeout        int               `json:"startupTimeout,omitempty"`  // milliseconds
	ShutdownTimeout       int               `json:"shutdownTimeout,omitempty"` // milliseconds
	RequestTimeout        int               `json:"requestTimeout,omitempty"`  // milliseconds
	RestartOnCrash        *bool             `json:"restartOnCrash,omitempty"`
	MaxRestarts           *int              `json:"maxRestarts,omitempty"`
	Diagnostics           *bool             `json:"diagnostics,omitempty"` // false keeps diagnostics out of edit results

	path string // the command as found on PATH
}

const (
	defaultStartupTimeout  = 30 * time.Second
	defaultShutdownTimeout = 5 * time.Second
	defaultRequestTimeout  = 60 * time.Second
	defaultMaxRestarts     = 3
)

// CommandLookup finds a server's command; tests replace it to ignore installed servers.
var CommandLookup = tooling.Resolve

// The definitions of Claude Code's official code intelligence plugins.
//
//go:embed servers.json
var builtinServers []byte

var defaultServers = func() []Server {
	servers, err := ParseServers(builtinServers)
	if err != nil {
		panic(err)
	}
	return servers
}()

// ParseServers reads a .lsp.json document and, like Claude Code, rejects unknown fields.
// Servers keep their order in the document: the first to claim an extension handles it.
func ParseServers(data []byte) ([]Server, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("language servers: expected an object of servers by name")
	}
	var servers []Server
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("language servers: %w", err)
		}
		name, _ := token.(string)
		var server Server
		if err := decoder.Decode(&server); err != nil {
			return nil, fmt.Errorf("language server %q: %w", name, err)
		}
		server.Name = name
		if err := server.validate(); err != nil {
			return nil, fmt.Errorf("language server %q: %w", name, err)
		}
		servers = ReplaceServer(servers, server)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("language servers: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("language servers: expected a single object")
	}
	return servers, nil
}

// ReplaceServer adds a server, or puts it in the place of an earlier one of its name.
func ReplaceServer(servers []Server, server Server) []Server {
	if index := slices.IndexFunc(servers, func(other Server) bool { return other.Name == server.Name }); index >= 0 {
		servers[index] = server
		return servers
	}
	return append(servers, server)
}

func (s Server) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("a name is required")
	}
	if strings.TrimSpace(s.Command) == "" {
		return fmt.Errorf("command is required")
	}
	if strings.ContainsAny(s.Command, " \t") && !strings.HasPrefix(s.Command, "/") {
		return fmt.Errorf("command must not contain spaces; pass arguments in args")
	}
	if len(s.ExtensionToLanguage) == 0 {
		return fmt.Errorf("extensionToLanguage is required")
	}
	for extension, language := range s.ExtensionToLanguage {
		if !strings.HasPrefix(extension, ".") || len(extension) < 2 || language == "" {
			return fmt.Errorf("extensionToLanguage maps dotted extensions to language IDs")
		}
	}
	if s.Transport != "" && s.Transport != "stdio" && s.Transport != "socket" {
		return fmt.Errorf("transport must be stdio or socket")
	}
	if s.StartupTimeout < 0 || s.ShutdownTimeout < 0 || s.RequestTimeout < 0 {
		return fmt.Errorf("timeouts must be positive milliseconds")
	}
	if s.MaxRestarts != nil && *s.MaxRestarts < 0 {
		return fmt.Errorf("maxRestarts must not be negative")
	}
	return nil
}

// An exact extension wins over a case-insensitive one: clangd tells ".C" from ".c".
func (s Server) LanguageIDForPath(path string) string {
	extension := filepath.Ext(path)
	if extension == "" {
		return ""
	}
	if language := s.ExtensionToLanguage[extension]; language != "" {
		return language
	}
	return s.ExtensionToLanguage[strings.ToLower(extension)]
}

func milliseconds(value int, fallback time.Duration) time.Duration {
	if value > 0 {
		return time.Duration(value) * time.Millisecond
	}
	return fallback
}

func (s Server) startupTimeout() time.Duration {
	return milliseconds(s.StartupTimeout, defaultStartupTimeout)
}

func (s Server) shutdownTimeout() time.Duration {
	return milliseconds(s.ShutdownTimeout, defaultShutdownTimeout)
}

func (s Server) requestTimeout() time.Duration {
	return milliseconds(s.RequestTimeout, defaultRequestTimeout)
}

func (s Server) restarts() int {
	switch {
	case s.RestartOnCrash != nil && !*s.RestartOnCrash:
		return 0
	case s.MaxRestarts != nil:
		return *s.MaxRestarts
	}
	return defaultMaxRestarts
}

func (s Server) ReportsDiagnostics() bool {
	return s.Diagnostics == nil || *s.Diagnostics
}

func (s Server) executable() string {
	if s.path != "" {
		return s.path
	}
	return s.Command
}

func cloneServer(server Server) Server {
	server.Args = slices.Clone(server.Args)
	server.ExtensionToLanguage = maps.Clone(server.ExtensionToLanguage)
	server.Env = maps.Clone(server.Env)
	server.InitializationOptions = slices.Clone(server.InitializationOptions)
	server.Settings = slices.Clone(server.Settings)
	if server.RestartOnCrash != nil {
		server.RestartOnCrash = new(*server.RestartOnCrash)
	}
	if server.MaxRestarts != nil {
		server.MaxRestarts = new(*server.MaxRestarts)
	}
	if server.Diagnostics != nil {
		server.Diagnostics = new(*server.Diagnostics)
	}
	return server
}
