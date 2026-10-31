package lsp

import (
	"context"
	"fmt"
	"net/url"
	"slices"

	"go.lsp.dev/protocol"
	lspuri "go.lsp.dev/uri"
)

// SupportsVirtualContent reports whether the server can read virtual
// documents of a scheme it returned, such as jdtls's jdt:// class files.
func (s *Session) SupportsVirtualContent(scheme string) bool {
	return s.advertisesContent(scheme) || s.classFileContents(scheme)
}

func (s *Session) advertisesContent(scheme string) bool {
	if s.capabilities.Workspace == nil {
		return false
	}
	switch options := s.capabilities.Workspace.TextDocumentContent.(type) {
	case *protocol.TextDocumentContentOptions:
		return slices.Contains(options.Schemes, scheme)
	case *protocol.TextDocumentContentRegistrationOptions:
		return slices.Contains(options.Schemes, scheme)
	}
	return false
}

// jdtls predates LSP 3.18 and serves class files through its own request.
func (s *Session) classFileContents(scheme string) bool {
	return scheme == "jdt" && s.server.Command == "jdtls"
}

// VirtualContent reads a virtual document through LSP 3.18
// workspace/textDocumentContent, or java/classFileContents for jdtls.
func (s *Session) VirtualContent(ctx context.Context, uri string) (string, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	switch {
	case s.advertisesContent(parsed.Scheme):
		result, err := s.rpc.TextDocumentContent(ctx, &protocol.TextDocumentContentParams{URI: lspuri.URI(uri)})
		if err != nil {
			return "", err
		}
		if result == nil {
			return "", fmt.Errorf("the language server returned no content for %s", uri)
		}
		return result.Text, nil
	case s.classFileContents(parsed.Scheme):
		var text string
		_, err := s.conn.Call(ctx, "java/classFileContents", protocol.TextDocumentIdentifier{URI: lspuri.URI(uri)}, &text)
		return text, err
	}
	return "", fmt.Errorf("the language server cannot read %s documents", parsed.Scheme)
}

// VirtualContent reads a virtual document from the first live session that
// supports its scheme.
func (m *Manager) VirtualContent(ctx context.Context, uri string) (string, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	var session *Session
	for _, entry := range m.sessions {
		if entry.session != nil && entry.session.IsAlive() && entry.session.SupportsVirtualContent(parsed.Scheme) {
			session = entry.session
			break
		}
	}
	m.mu.Unlock()
	if session == nil {
		return "", fmt.Errorf("no language server can read %s documents", parsed.Scheme)
	}
	return session.VirtualContent(ctx, uri)
}
