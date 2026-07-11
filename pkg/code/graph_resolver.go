package code

import (
	"context"
	"path/filepath"
)

type lspResolver struct {
	ws *Workspace
}

func (r *lspResolver) ResolveCall(ctx context.Context, file string, line, column int) (string, int, bool) {
	r.ws.lspLifeMu.RLock()
	defer r.ws.lspLifeMu.RUnlock()
	r.ws.mu.RLock()
	mgr := r.ws.LSP
	r.ws.mu.RUnlock()
	if mgr == nil {
		return "", 0, false
	}

	abs := filepath.Join(mgr.WorkingDir(), filepath.FromSlash(file))
	if mgr.FindServer(abs) == nil {
		return "", 0, false
	}

	session, err := mgr.GetSession(ctx, abs)
	if err != nil {
		return "", 0, false
	}

	uri, err := session.OpenDocument(ctx, abs)
	if err != nil {
		return "", 0, false
	}

	defs, err := session.DefinitionLocations(ctx, uri, line, column)
	if err != nil || len(defs) == 0 {
		return "", 0, false
	}

	rel, err := filepath.Rel(mgr.WorkingDir(), defs[0].Path)
	if err != nil {
		return "", 0, false
	}
	return filepath.ToSlash(rel), defs[0].Line + 1, true
}
