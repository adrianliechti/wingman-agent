package language

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func HoverText(hover *lsp.Hover) string {
	if hover == nil {
		return ""
	}
	switch contents := hover.Contents.(type) {
	case lsp.String:
		return string(contents)
	case *lsp.MarkupContent:
		return contents.Value
	case *lsp.MarkedStringWithLanguage:
		return contents.Value
	case lsp.MarkedStringSlice:
		parts := make([]string, 0, len(contents))
		for _, marked := range contents {
			switch marked := marked.(type) {
			case lsp.String:
				parts = append(parts, string(marked))
			case *lsp.MarkedStringWithLanguage:
				parts = append(parts, marked.Value)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func DocumentSymbolsFromProtocol(result lsp.DocumentSymbolResult) ([]lsp.DocumentSymbol, []lsp.SymbolInformation) {
	switch result := result.(type) {
	case lsp.DocumentSymbolSlice:
		return []lsp.DocumentSymbol(result), nil
	case lsp.SymbolInformationSlice:
		return nil, []lsp.SymbolInformation(result)
	default:
		return nil, nil
	}
}

// Without a file, at most this many entries are walked to find the workspace's languages.
const maxLanguageScan = 20000

// WorkspaceSymbols searches each installed language found in the workspace.
func (s *Service) WorkspaceSymbols(ctx context.Context, query string) ([]lsp.WorkspaceSymbol, error) {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed {
		return nil, fmt.Errorf("language service is closed")
	}
	paths := s.languageFiles(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no file in the workspace has an installed language server; pass filePath")
	}
	var symbols []lsp.WorkspaceSymbol
	var failures []error
	for _, path := range paths {
		session, err := s.manager.GetSession(ctx, path)
		if err == nil {
			if lsp.CapabilityEnabled(session.Capabilities().WorkspaceSymbolProvider) {
				var found []lsp.WorkspaceSymbol
				found, err = session.WorkspaceSymbols(ctx, query)
				symbols = append(symbols, found...)
			} else {
				err = fmt.Errorf("%s does not support workspace symbols", filepath.Ext(path))
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == len(paths) {
		return nil, errors.Join(failures...)
	}
	return symbols, nil
}

// languageFiles returns one file for each language server that handles files in the workspace.
func (s *Service) languageFiles(ctx context.Context) []string {
	var paths []string
	servers := map[string]bool{}
	seen := map[string]bool{}
	entries := 0
	_ = pathutil.WalkDir(s.root, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			return filepath.SkipDir
		}
		if entries++; entries > maxLanguageScan {
			return filepath.SkipAll
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != s.root && (strings.HasPrefix(name, ".") || skippedDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		extension := filepath.Ext(name)
		if extension == "" || seen[extension] {
			return nil
		}
		seen[extension] = true
		if server := s.manager.FindServer(path); server != nil && !servers[server.Name] {
			servers[server.Name] = true
			paths = append(paths, path)
		}
		return nil
	})
	return paths
}

var skippedDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true, "build": true, "dist": true,
	"out": true, "bin": true, "obj": true, "__pycache__": true, "venv": true,
}
