package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

type editorCapabilitiesResponse struct {
	WorkspaceURI                      string   `json:"workspace_uri"`
	LanguageServer                    bool     `json:"language_server"`
	Completion                        bool     `json:"completion"`
	CompletionResolve                 bool     `json:"completion_resolve"`
	CompletionTriggerCharacters       []string `json:"completion_trigger_characters"`
	SignatureHelp                     bool     `json:"signature_help"`
	SignatureHelpTriggerCharacters    []string `json:"signature_help_trigger_characters"`
	SignatureHelpRetriggerCharacters  []string `json:"signature_help_retrigger_characters"`
	Hover                             bool     `json:"hover"`
	Definition                        bool     `json:"definition"`
	TypeDefinition                    bool     `json:"type_definition"`
	References                        bool     `json:"references"`
	Implementation                    bool     `json:"implementation"`
	DocumentSymbols                   bool     `json:"document_symbols"`
	DocumentHighlights                bool     `json:"document_highlights"`
	FoldingRanges                     bool     `json:"folding_ranges"`
	Rename                            bool     `json:"rename"`
	RenamePrepare                     bool     `json:"rename_prepare"`
	CodeActions                       bool     `json:"code_actions"`
	CodeActionResolve                 bool     `json:"code_action_resolve"`
	DocumentFormatting                bool     `json:"document_formatting"`
	RangeFormatting                   bool     `json:"range_formatting"`
	OnTypeFormattingTriggerCharacters []string `json:"on_type_formatting_trigger_characters"`
	SemanticTokens                    bool     `json:"semantic_tokens"`
	InlayHints                        bool     `json:"inlay_hints"`
}

type lspActivityResponse struct {
	Analyzing bool                 `json:"analyzing"`
	Services  []lspServiceActivity `json:"services"`
}

type lspServiceActivity struct {
	Server     string             `json:"server"`
	Label      string             `json:"label"`
	Project    string             `json:"project"`
	Analyzing  bool               `json:"analyzing"`
	Operations []lsp.WorkProgress `json:"operations"`
}

func (s *Server) lspActivity() lspActivityResponse {
	activities := s.workspace.LSPActivities()
	services := make([]lspServiceActivity, 0, len(activities))
	analyzing := false
	for _, activity := range activities {
		project, err := filepath.Rel(s.workspace.RootPath, activity.ProjectDir)
		if err != nil || project == ".." || filepath.IsAbs(project) || strings.HasPrefix(project, ".."+string(filepath.Separator)) {
			project = filepath.Base(activity.ProjectDir)
		}
		project = filepath.ToSlash(project)
		services = append(services, lspServiceActivity{
			Server:     activity.Server,
			Label:      activity.Label,
			Project:    project,
			Analyzing:  activity.Analyzing,
			Operations: activity.Operations,
		})
		analyzing = analyzing || activity.Analyzing
	}
	return lspActivityResponse{Analyzing: analyzing, Services: services}
}

func (s *Server) lspEditorCapabilities(ctx context.Context, filePath string) editorCapabilitiesResponse {
	capabilities, hasServer, err := s.workspace.EditorLSPCapabilities(ctx, filePath)
	if err != nil {
		// A missing or failed server must not disable structural fallbacks.
		hasServer = false
	}
	response := editorCapabilitiesResponse{
		WorkspaceURI:       fileuri.FromPath(s.workspace.RootPath),
		LanguageServer:     hasServer,
		Completion:         true,
		Hover:              true,
		Definition:         true,
		References:         true,
		Implementation:     true,
		DocumentSymbols:    true,
		DocumentHighlights: true,
		FoldingRanges:      true,
		SemanticTokens:     true,
	}
	if hasServer {
		if provider := capabilities.CompletionProvider; provider != nil {
			response.CompletionResolve = provider.ResolveProvider != nil && *provider.ResolveProvider
			response.CompletionTriggerCharacters = nonNilStrings(provider.TriggerCharacters)
		}
		if provider := capabilities.SignatureHelpProvider; provider != nil {
			response.SignatureHelp = true
			response.SignatureHelpTriggerCharacters = nonNilStrings(provider.TriggerCharacters)
			response.SignatureHelpRetriggerCharacters = nonNilStrings(provider.RetriggerCharacters)
		}
		// Structural features remain available when the server omits them or
		// fails later; only type-aware features are strictly capability-gated.
		response.TypeDefinition = lsp.CapabilityEnabled(capabilities.TypeDefinitionProvider)
		response.Rename, response.RenamePrepare = renameCapabilities(capabilities.RenameProvider)
		response.CodeActions, response.CodeActionResolve = codeActionCapabilities(capabilities.CodeActionProvider)
		response.DocumentFormatting = lsp.CapabilityEnabled(capabilities.DocumentFormattingProvider)
		response.RangeFormatting = lsp.CapabilityEnabled(capabilities.DocumentRangeFormattingProvider)
		if provider := capabilities.DocumentOnTypeFormattingProvider; provider.FirstTriggerCharacter != "" {
			response.OnTypeFormattingTriggerCharacters = append(
				[]string{provider.FirstTriggerCharacter},
				provider.MoreTriggerCharacter...,
			)
		}
		response.SemanticTokens = true
		response.InlayHints = lsp.CapabilityEnabled(capabilities.InlayHintProvider)
	}
	return response
}

func renameCapabilities(value any) (enabled, prepare bool) {
	switch provider := value.(type) {
	case lsp.Boolean:
		return bool(provider), false
	case *lsp.RenameOptions:
		return true, provider.PrepareProvider != nil && *provider.PrepareProvider
	default:
		return false, false
	}
}

func codeActionCapabilities(value any) (enabled, resolve bool) {
	switch provider := value.(type) {
	case lsp.Boolean:
		return bool(provider), false
	case *lsp.CodeActionOptions:
		return true, provider.ResolveProvider != nil && *provider.ResolveProvider
	default:
		return false, false
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// allowLSPExternalFile records a language-server reported location outside the
// workspace so workspace/textDocumentContent may serve exactly those files, read-only.
func (s *Server) allowLSPExternalFile(path string) {
	s.lspExternalMu.Lock()
	defer s.lspExternalMu.Unlock()
	if s.lspExternalPaths == nil {
		s.lspExternalPaths = make(map[string]bool)
	}
	s.lspExternalPaths[path] = true
}

func (s *Server) lspExternalFileAllowed(path string) bool {
	s.lspExternalMu.Lock()
	defer s.lspExternalMu.Unlock()
	return s.lspExternalPaths[path]
}

type lspWorkspaceDocument struct {
	Path string `json:"path"`
	// Revision identifies the exact content the language server used. For an
	// open document that can intentionally differ from the disk revision.
	Revision string `json:"revision,omitempty"`
	Exists   bool   `json:"exists"`
}

func (s *Server) workspaceEditDocuments(edits ...*lsp.WorkspaceEdit) (map[string]lspWorkspaceDocument, error) {
	documents := make(map[string]lspWorkspaceDocument)
	for _, edit := range edits {
		for _, key := range workspaceEditURIs(edit) {
			if _, exists := documents[key]; exists {
				continue
			}
			filePath, ok := fileuri.Path(key)
			if !ok {
				return nil, fmt.Errorf("language server edit targets a non-file URI")
			}
			rel, err := filepath.Rel(s.workspace.RootPath, filePath)
			if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("language server edit targets a file outside the workspace")
			}
			content, err := s.workspace.Root.ReadFile(rel)
			switch {
			case err == nil:
				document := lspWorkspaceDocument{Path: filepath.ToSlash(rel), Revision: fileRevision(content), Exists: true}
				if buffer, ok := s.workspace.EditorLSPDocumentContent(filePath); ok {
					document.Revision = fileRevision([]byte(buffer))
				}
				documents[key] = document
			case errors.Is(err, fs.ErrNotExist):
				documents[key] = lspWorkspaceDocument{Path: filepath.ToSlash(rel), Exists: false}
			default:
				return nil, err
			}
		}
	}
	return documents, nil
}

func workspaceEditURIs(edit *lsp.WorkspaceEdit) []string {
	if edit == nil {
		return nil
	}
	result := make([]string, 0, len(edit.Changes)+len(edit.DocumentChanges))
	if edit.DocumentChanges != nil {
		for _, change := range edit.DocumentChanges {
			switch change := change.(type) {
			case *lsp.TextDocumentEdit:
				result = append(result, change.TextDocument.URI.String())
			case *lsp.CreateFile:
				result = append(result, change.URI.String())
			case *lsp.RenameFile:
				result = append(result, change.OldURI.String(), change.NewURI.String())
			case *lsp.DeleteFile:
				result = append(result, change.URI.String())
			}
		}
		return result
	}
	for documentURI := range edit.Changes {
		result = append(result, documentURI.String())
	}
	return result
}
