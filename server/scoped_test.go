package server

import "net/http"

func serveTestHTTP(app *Server, w http.ResponseWriter, r *http.Request) {
	r.Header.Set(instanceHeader, app.scope.InstanceID)
	if handler := legacyEditorTestHandler(app, r.Method+" "+r.URL.Path); handler != nil {
		handler(w, r)
		return
	}
	app.ServeHTTP(w, r)
}
func scopedTestHandler(app *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveTestHTTP(app, w, r) })
}

// These adapters are test-only entry points into validation and workspace services.
func legacyEditorTestHandler(app *Server, route string) http.HandlerFunc {
	switch route {
	case "GET /api/lsp/status":
		return app.handleLSPStatus
	case "GET /api/lsp/capabilities":
		return app.handleLSPEditorCapabilities
	case "POST /api/lsp/document":
		return app.handleLSPDocumentLifecycle
	case "GET /api/lsp/diagnostics":
		return app.handleDiagnostics
	case "POST /api/lsp/diagnostics":
		return app.handleLSPFileDiagnostics
	case "POST /api/lsp/definition":
		return app.handleLSPDefinition
	case "POST /api/lsp/type-definition":
		return app.handleLSPTypeDefinition
	case "POST /api/lsp/implementations":
		return app.handleLSPImplementations
	case "POST /api/lsp/references":
		return app.handleLSPReferences
	case "POST /api/lsp/hover":
		return app.handleLSPHover
	case "POST /api/lsp/completions":
		return app.handleLSPCompletions
	case "POST /api/lsp/completions/resolve":
		return app.handleLSPCompletionResolve
	case "POST /api/lsp/signature-help":
		return app.handleLSPSignatureHelp
	case "POST /api/lsp/document-symbols":
		return app.handleLSPDocumentSymbols
	case "POST /api/lsp/document-highlights":
		return app.handleLSPDocumentHighlights
	case "POST /api/lsp/folding-ranges":
		return app.handleLSPFoldingRanges
	case "POST /api/lsp/semantic-tokens":
		return app.handleLSPSemanticTokens
	case "POST /api/lsp/rename/prepare":
		return app.handleLSPPrepareRename
	case "POST /api/lsp/rename":
		return app.handleLSPRename
	case "POST /api/lsp/code-actions":
		return app.handleLSPCodeActions
	case "POST /api/lsp/code-actions/resolve":
		return app.handleLSPCodeActionResolve
	case "POST /api/lsp/execute-command":
		return app.handleLSPExecuteCommand
	case "POST /api/lsp/formatting":
		return app.handleLSPFormatting
	case "POST /api/lsp/formatting/range":
		return app.handleLSPRangeFormatting
	case "POST /api/lsp/formatting/on-type":
		return app.handleLSPOnTypeFormatting
	case "POST /api/lsp/inlay-hints":
		return app.handleLSPInlayHints
	case "POST /api/debug/start":
		return app.handleDebugStart
	case "GET /api/debug/session":
		return app.handleDebugSession
	case "GET /api/debug/state":
		return app.handleDebugState
	case "GET /api/debug/inspection":
		return app.handleDebugInspection
	case "PUT /api/debug/breakpoints":
		return app.handleDebugBreakpoints
	case "POST /api/debug/control":
		return app.handleDebugControl
	case "POST /api/debug/evaluate":
		return app.handleDebugEvaluate
	case "POST /api/debug/scopes":
		return app.handleDebugScopes
	case "POST /api/debug/variables":
		return app.handleDebugVariables
	}
	return nil
}
