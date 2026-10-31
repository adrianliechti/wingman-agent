package server

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

type lspFeatureParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"position"`
	Range     lsp.Range             `json:"range"`
	Options   lsp.FormattingOptions `json:"options"`
	Context   json.RawMessage       `json:"context"`
	NewName   string                `json:"newName"`
	Character string                `json:"ch"`
	Command   string                `json:"command"`
	Arguments []lsp.LSPAny          `json:"arguments"`
	Data      json.RawMessage       `json:"data"`
}

// lspQuery is a document request after routing and workspace confinement.
type lspQuery struct {
	id      json.RawMessage
	raw     json.RawMessage
	params  lspFeatureParams
	uri     string
	path    string
	content *string
	line    int
	column  int
	buffers map[string]lspBuffer
}

type lspMethod struct {
	handle func(*lspConnection, context.Context, lspQuery) (any, error)
	// empty marks a type-aware method: its result for a document that no
	// language server covers. Other methods have structural fallbacks.
	empty func(lspQuery) any
	// resolve methods find their document in the item's routing data.
	resolve bool
}

var lspWorkspaceMethods = map[string]func(*lspConnection, context.Context, json.RawMessage) (any, error){
	"workspace/textDocumentContent": (*lspConnection).textDocumentContent,
	"workspace/diagnostic":          (*lspConnection).workspaceDiagnostics,
}

var lspDocumentMethods = map[string]lspMethod{
	"wingman/documentCapabilities": {handle: func(c *lspConnection, ctx context.Context, q lspQuery) (any, error) {
		return c.server.lspEditorCapabilities(ctx, q.path), nil
	}},
	"textDocument/diagnostic":          {handle: (*lspConnection).diagnostic, empty: lspEmptyDiagnostics},
	"textDocument/definition":          {handle: lspLocations((*code.Workspace).DefinitionLocations)},
	"textDocument/typeDefinition":      {handle: lspLocations((*code.Workspace).TypeDefinitionLocations)},
	"textDocument/implementation":      {handle: lspLocations((*code.Workspace).ImplementationLocations)},
	"textDocument/references":          {handle: (*lspConnection).references},
	"textDocument/hover":               {handle: (*lspConnection).hover},
	"textDocument/completion":          {handle: (*lspConnection).completion},
	"completionItem/resolve":           {handle: (*lspConnection).resolveCompletion, empty: lspUnchanged, resolve: true},
	"textDocument/signatureHelp":       {handle: (*lspConnection).signatureHelp, empty: lspNull},
	"textDocument/documentSymbol":      {handle: (*lspConnection).documentSymbols},
	"textDocument/documentHighlight":   {handle: (*lspConnection).documentHighlights},
	"textDocument/foldingRange":        {handle: (*lspConnection).foldingRanges},
	"textDocument/semanticTokens/full": {handle: (*lspConnection).semanticTokens},
	"textDocument/prepareRename":       {handle: (*lspConnection).prepareRename, empty: lspNull},
	"textDocument/rename":              {handle: (*lspConnection).rename, empty: lspNull},
	"textDocument/codeAction":          {handle: (*lspConnection).codeActions, empty: func(lspQuery) any { return []any{} }},
	"codeAction/resolve":               {handle: (*lspConnection).resolveCodeAction, empty: lspUnchanged, resolve: true},
	"workspace/executeCommand":         {handle: (*lspConnection).executeCommand},
	"textDocument/formatting":          {handle: (*lspConnection).formatting, empty: lspNull},
	"textDocument/rangeFormatting":     {handle: (*lspConnection).rangeFormatting, empty: lspNull},
	"textDocument/onTypeFormatting":    {handle: (*lspConnection).onTypeFormatting, empty: lspNull},
	"textDocument/inlayHint":           {handle: (*lspConnection).inlayHints, empty: lspNull},
}

func lspNull(lspQuery) any             { return nil }
func lspUnchanged(q lspQuery) any      { return q.raw }
func lspEmptyDiagnostics(lspQuery) any { return map[string]any{"kind": "full", "items": []any{}} }

func (c *lspConnection) request(ctx context.Context, req ahpRequest, buffers map[string]lspBuffer) (any, error) {
	if handle := lspWorkspaceMethods[req.Method]; handle != nil {
		return handle(c, ctx, req.Params)
	}
	method, ok := lspDocumentMethods[req.Method]
	if !ok {
		return nil, &ahpRPCError{Code: -32601, Message: "unsupported language method: " + req.Method}
	}
	q := lspQuery{id: req.ID, raw: req.Params, buffers: buffers}
	if err := lsp.Unmarshal(req.Params, &q.params); err != nil {
		return nil, ahpInvalid("invalid language request")
	}
	q.uri = q.params.TextDocument.URI
	if method.resolve {
		uri, raw, err := unwrapLSPResolve(q.params.Data, req.Params)
		if err != nil {
			return nil, err
		}
		q.uri, q.raw = uri, raw
	}
	var err error
	if q.path, err = c.path(q.uri); err != nil {
		return nil, err
	}
	if method.empty != nil && !c.server.workspace.HasLSPFor(q.path) {
		return method.empty(q), nil
	}
	if doc, ok := buffers[q.uri]; ok {
		q.content = &doc.text
	}
	q.line, q.column = q.params.Position.Line, q.params.Position.Character
	if q.line < 0 || q.column < 0 {
		return nil, ahpInvalid("negative document position")
	}
	return method.handle(c, ctx, q)
}

// Resolve data is opaque to clients. It wraps rather than overwrites the
// language server's own data and records the item's document.
func unwrapLSPResolve(data, params json.RawMessage) (string, json.RawMessage, error) {
	var route struct {
		URI  string          `json:"uri"`
		Data json.RawMessage `json:"data"`
	}
	var wrapper map[string]json.RawMessage
	if json.Unmarshal(data, &wrapper) != nil || json.Unmarshal(wrapper["wingman"], &route) != nil || route.URI == "" {
		return "", nil, ahpInvalid("missing resolve routing data")
	}
	var item map[string]json.RawMessage
	_ = json.Unmarshal(params, &item)
	if len(route.Data) == 0 {
		delete(item, "data")
	} else {
		item["data"] = route.Data
	}
	raw, err := json.Marshal(item)
	return route.URI, raw, err
}

func (c *lspConnection) textDocumentContent(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil, ahpInvalid("invalid content request")
	}
	path, virtual, ok := c.server.lspSourceTarget(p.URI)
	if !ok {
		return nil, &ahpRPCError{Code: -32803, Message: "source not found"}
	}
	if virtual != "" {
		text, err := c.server.workspace.LSPVirtualContent(ctx, virtual)
		if err != nil {
			return nil, err
		}
		return map[string]string{"text": text}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > ahpResourceLimit {
		return nil, ahpInvalid("source is not a readable text file")
	}
	data, err := io.ReadAll(io.LimitReader(f, ahpResourceLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > ahpResourceLimit || isBinary(data) || !utf8.Valid(data) {
		return nil, ahpInvalid("source is not a readable text file")
	}
	return map[string]string{"text": string(data)}, nil
}

func (c *lspConnection) workspaceDiagnostics(ctx context.Context, _ json.RawMessage) (any, error) {
	report := c.server.workspace.Diagnostics(ctx)
	items := []any{}
	reported := map[string]bool{}
	for path, diagnostics := range report.Diagnostics {
		uri := fileuri.FromPath(path)
		reported[uri] = true
		items = append(items, map[string]any{"uri": uri, "version": nil, "kind": "full", "items": lspDiagnostics(diagnostics)})
	}
	c.mu.Lock()
	// A file reported earlier but clean now needs an empty report to clear it.
	for uri := range c.reported {
		if !reported[uri] {
			items = append(items, map[string]any{"uri": uri, "version": nil, "kind": "full", "items": []any{}})
		}
	}
	c.reported = reported
	c.mu.Unlock()
	return map[string]any{"items": items, "_wingman": map[string]any{"checked_files": report.CheckedFiles, "discovered_files": report.DiscoveredFiles, "discovery_truncated": report.DiscoveryTruncated, "unknown_files": report.UnknownFiles, "unavailable_servers": report.UnavailableServers, "analyzing": report.Analyzing}}, nil
}

func (c *lspConnection) diagnostic(ctx context.Context, q lspQuery) (any, error) {
	diagnostics, known, err := c.server.workspace.FileDiagnostics(ctx, q.path, q.content)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, &ahpRPCError{Code: -32802, Message: "diagnostics are not available yet", Data: map[string]any{"retriggerRequest": true}}
	}
	return map[string]any{"kind": "full", "items": lspDiagnostics(diagnostics)}, nil
}

type lspLocationFinder func(*code.Workspace, context.Context, string, *string, int, int) ([]lsp.Location, error)

func lspLocations(find lspLocationFinder) func(*lspConnection, context.Context, lspQuery) (any, error) {
	return func(c *lspConnection, ctx context.Context, q lspQuery) (any, error) {
		locations, err := find(c.server.workspace, ctx, q.path, q.content, q.line, q.column)
		return c.allowLocations(locations), err
	}
}

// Locations outside the workspace become wingman-source documents, readable
// through workspace/textDocumentContent only once a language server reported them.
func (c *lspConnection) allowLocations(locations []lsp.Location) []lsp.Location {
	for i := range locations {
		if source, ok := c.server.lspSource(string(locations[i].URI)); ok {
			locations[i].URI = lsp.DocumentURI(source)
		}
	}
	if locations == nil {
		return []lsp.Location{}
	}
	return locations
}

func (c *lspConnection) references(ctx context.Context, q lspQuery) (any, error) {
	var reference struct {
		IncludeDeclaration bool `json:"includeDeclaration"`
	}
	if len(q.params.Context) > 0 && json.Unmarshal(q.params.Context, &reference) != nil {
		return nil, ahpInvalid("invalid reference context")
	}
	locations, err := c.server.workspace.ReferenceLocations(ctx, q.path, q.content, q.line, q.column, reference.IncludeDeclaration)
	return c.allowLocations(locations), err
}

func (c *lspConnection) hover(ctx context.Context, q lspQuery) (any, error) {
	text, textRange, err := c.server.workspace.HoverRange(ctx, q.path, q.content, q.line, q.column)
	if err != nil || strings.TrimSpace(text) == "" {
		return nil, err
	}
	hover := map[string]any{"contents": map[string]string{"kind": "markdown", "value": text}}
	if textRange != nil {
		hover["range"] = textRange
	}
	return hover, nil
}

func (c *lspConnection) completion(ctx context.Context, q lspQuery) (any, error) {
	var trigger *lsp.CompletionContext
	if len(q.params.Context) > 0 {
		if err := lsp.Unmarshal(q.params.Context, &trigger); err != nil {
			return nil, err
		}
	}
	list, err := c.server.workspace.CompletionItems(ctx, q.path, q.content, q.line, q.column, trigger)
	if err != nil {
		return nil, err
	}
	if list.Items == nil {
		list.Items = []lsp.CompletionItem{}
	}
	return routeLSPItems(list, q.uri, "items")
}

func (c *lspConnection) resolveCompletion(ctx context.Context, q lspQuery) (any, error) {
	var item lsp.CompletionItem
	if err := lsp.Unmarshal(q.raw, &item); err != nil {
		return nil, err
	}
	return c.server.workspace.ResolveCompletionItem(ctx, q.path, q.content, item)
}

func (c *lspConnection) signatureHelp(ctx context.Context, q lspQuery) (any, error) {
	var trigger *lsp.SignatureHelpContext
	if len(q.params.Context) > 0 {
		if err := lsp.Unmarshal(q.params.Context, &trigger); err != nil {
			return nil, err
		}
	}
	return c.server.workspace.SignatureHelp(ctx, q.path, q.content, q.line, q.column, trigger)
}

func (c *lspConnection) documentSymbols(ctx context.Context, q lspQuery) (any, error) {
	documents, flat, err := c.server.workspace.DocumentSymbolItems(ctx, q.path, q.content)
	if len(documents) > 0 {
		return documents, err
	}
	converted := []lsp.DocumentSymbol{}
	for _, symbol := range flat {
		converted = append(converted, lsp.DocumentSymbol{Name: symbol.Name, Kind: symbol.Kind, Range: symbol.Location.Range, SelectionRange: symbol.Location.Range})
	}
	return converted, err
}

func (c *lspConnection) documentHighlights(ctx context.Context, q lspQuery) (any, error) {
	return c.server.workspace.DocumentHighlights(ctx, q.path, q.content, q.line, q.column)
}

func (c *lspConnection) foldingRanges(ctx context.Context, q lspQuery) (any, error) {
	return c.server.workspace.FoldingRanges(ctx, q.path, q.content)
}

func (c *lspConnection) semanticTokens(ctx context.Context, q lspQuery) (any, error) {
	tokens, err := c.server.workspace.SemanticTokens(ctx, q.path, q.content)
	if err != nil {
		return nil, err
	}
	return encodeLSPSemanticTokens(tokens), nil
}

func (c *lspConnection) prepareRename(ctx context.Context, q lspQuery) (any, error) {
	return c.server.workspace.PrepareRename(ctx, q.path, q.content, q.line, q.column)
}

func (c *lspConnection) rename(ctx context.Context, q lspQuery) (any, error) {
	if strings.TrimSpace(q.params.NewName) == "" {
		return nil, ahpInvalid("newName is required")
	}
	edit, err := c.server.workspace.Rename(ctx, q.path, q.content, q.line, q.column, q.params.NewName)
	if err != nil {
		return nil, err
	}
	return c.lspEdit(edit, q.buffers)
}

func (c *lspConnection) codeActions(ctx context.Context, q lspQuery) (any, error) {
	var actionContext struct {
		Only        []lsp.CodeActionKind      `json:"only"`
		TriggerKind lsp.CodeActionTriggerKind `json:"triggerKind"`
	}
	if len(q.params.Context) > 0 {
		if err := json.Unmarshal(q.params.Context, &actionContext); err != nil {
			return nil, err
		}
	}
	actions, err := c.server.workspace.CodeActions(ctx, q.path, q.content, q.params.Range, actionContext.Only, actionContext.TriggerKind)
	if err != nil {
		return nil, err
	}
	if actions == nil {
		actions = []lsp.CommandOrCodeAction{}
	}
	result, err := routeLSPItems(actions, q.uri, "")
	if err != nil {
		return nil, err
	}
	for _, value := range result.([]any) {
		action := value.(map[string]any)
		if edit, ok := action["edit"]; ok {
			data, _ := json.Marshal(edit)
			var parsed lsp.WorkspaceEdit
			if err := lsp.Unmarshal(data, &parsed); err != nil {
				return nil, err
			}
			if action["edit"], err = c.lspEdit(&parsed, q.buffers); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func (c *lspConnection) resolveCodeAction(ctx context.Context, q lspQuery) (any, error) {
	var action lsp.CodeAction
	if err := lsp.Unmarshal(q.raw, &action); err != nil {
		return nil, err
	}
	resolved, err := c.server.workspace.ResolveCodeAction(ctx, q.path, q.content, action)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		resolved = &action
	}
	data, err := lsp.Marshal(resolved)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	if resolved.Edit != nil {
		result["edit"], err = c.lspEdit(resolved.Edit, q.buffers)
	}
	return result, err
}

func (c *lspConnection) executeCommand(ctx context.Context, q lspQuery) (any, error) {
	w := c.server.workspace
	if q.params.Command == "" {
		return nil, ahpInvalid("command is required")
	}
	capabilities, _, err := w.EditorLSPCapabilities(ctx, q.path)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(capabilities.ExecuteCommandProvider.Commands, q.params.Command) {
		return nil, ahpInvalid("the language server does not provide command " + q.params.Command)
	}
	// The native call stays bounded, except while the user reviews an edit.
	native, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(2*time.Minute, func() { cancel(errEditorTimeout) })
	defer timer.Stop()
	apply := func(requested lsp.CommandWorkspaceEdit) bool {
		timer.Stop()
		defer timer.Reset(2 * time.Minute)
		edit, err := c.lspEdit(&requested.Edit, q.buffers)
		if err != nil {
			return false
		}
		applied, err := c.applyEdit(ctx, q.id, requested.Label, edit)
		return err == nil && applied
	}
	result, err := w.ExecuteLSPCommandWithEdits(native, q.path, q.content, lsp.Command{Command: q.params.Command, Arguments: q.params.Arguments}, apply)
	return result, editorRequestError(native, err)
}

func (c *lspConnection) formatting(ctx context.Context, q lspQuery) (any, error) {
	return c.server.workspace.Formatting(ctx, q.path, q.content, q.params.Options)
}

func (c *lspConnection) rangeFormatting(ctx context.Context, q lspQuery) (any, error) {
	return c.server.workspace.RangeFormatting(ctx, q.path, q.content, q.params.Range, q.params.Options)
}

func (c *lspConnection) onTypeFormatting(ctx context.Context, q lspQuery) (any, error) {
	return c.server.workspace.OnTypeFormatting(ctx, q.path, q.content, q.line, q.column, q.params.Character, q.params.Options)
}

func (c *lspConnection) inlayHints(ctx context.Context, q lspQuery) (any, error) {
	return c.server.workspace.InlayHints(ctx, q.path, q.content, q.params.Range)
}

func routeLSPItems(value any, uri, field string) (any, error) {
	data, err := lsp.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	items := result
	if field != "" {
		items = result.(map[string]any)[field]
	}
	for _, value := range items.([]any) {
		item := value.(map[string]any)
		item["data"] = map[string]any{"wingman": map[string]any{"uri": uri, "data": item["data"]}}
	}
	return result, nil
}

// lspEdit labels document edits with this editor's versions; the language
// server's own version counter means nothing to the client.
func (c *lspConnection) lspEdit(edit *lsp.WorkspaceEdit, buffers map[string]lspBuffer) (any, error) {
	if edit == nil {
		return nil, nil
	}
	documents, err := c.server.workspaceEditDocuments(edit)
	if err != nil {
		return nil, err
	}
	data, err := lsp.Marshal(edit)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	changes, _ := result["documentChanges"].([]any)
	for _, change := range changes {
		document, _ := change.(map[string]any)["textDocument"].(map[string]any)
		if document == nil {
			continue
		}
		uri, _ := document["uri"].(string)
		if buffer, ok := buffers[uri]; ok {
			document["version"] = buffer.version
		} else {
			document["version"] = nil
		}
	}
	result["_wingman"] = map[string]any{"documents": documents}
	return result, nil
}

func lspDiagnostics(diagnostics []language.Diagnostic) []any {
	items := make([]any, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		if len(diagnostic.Wire) > 0 {
			items = append(items, diagnostic.Wire)
		} else {
			items = append(items, diagnostic)
		}
	}
	return items
}
func encodeLSPSemanticTokens(tokens []language.SemanticToken) map[string]any {
	slices.SortStableFunc(tokens, func(a, b language.SemanticToken) int {
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Character - b.Character
	})
	data := []int{}
	line, char := 0, 0
	for _, token := range tokens {
		kind := slices.Index(lspTokenTypes, token.Type)
		if kind < 0 || token.Length <= 0 || token.Line < 0 || token.Character < 0 {
			continue
		}
		deltaLine, deltaChar := token.Line-line, token.Character
		if deltaLine == 0 {
			deltaChar -= char
		}
		flags := 0
		for _, modifier := range token.Modifiers {
			if i := slices.Index(lspTokenModifiers, modifier); i >= 0 {
				flags |= 1 << i
			}
		}
		data = append(data, deltaLine, deltaChar, token.Length, kind, flags)
		line, char = token.Line, token.Character
	}
	return map[string]any{"data": data}
}
