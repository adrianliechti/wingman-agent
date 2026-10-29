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
	case "GET /api/files":
		return app.handleFiles
	case "POST /api/files":
		return app.handleFileCreate
	case "DELETE /api/files":
		return app.handleFileDelete
	case "GET /api/files/read":
		return app.handleFileRead
	case "GET /api/files/search":
		return app.handleFilesSearch
	case "POST /api/files/content-search":
		return app.handleWorkspaceSearch
	case "GET /api/files/path":
		return app.handleFilePath
	case "GET /api/files/download":
		return app.handleFileDownload
	case "GET /api/files/preview":
		return app.handleFilePreview
	case "POST /api/files/reveal":
		return app.handleFileReveal
	case "POST /api/files/rename":
		return app.handleFileRename
	case "POST /api/files/copy":
		return app.handleFileCopy
	case "POST /api/files/write":
		return app.handleFileWrite
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
