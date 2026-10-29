package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

var lspTokenTypes = strings.Fields("namespace type class enum interface struct typeParameter parameter variable property enumMember event function method macro label comment string keyword number regexp operator decorator")
var lspTokenModifiers = strings.Fields("declaration definition readonly static deprecated abstract async modification documentation defaultLibrary")

type lspBuffer struct {
	path    string
	text    string
	version int
}
type lspConnection struct {
	*editorSocket
	server    *Server
	mu        sync.Mutex
	documents map[string]lspBuffer
	replies   map[string]chan json.RawMessage
	sequence  atomic.Uint64
}

func (s *Server) handleLSP(w http.ResponseWriter, r *http.Request) {
	peer, err := s.acceptEditorSocket(w, r)
	if err != nil {
		return
	}
	defer peer.close()
	c := &lspConnection{editorSocket: peer, server: s, documents: map[string]lspBuffer{}, replies: map[string]chan json.RawMessage{}}
	// Document notifications are processed in receive order. Requests begin only
	// after preceding changes have been applied; cancellation never queues behind
	// a slow language server.
	type task struct {
		ahpRequest
		ctx  context.Context
		done func()
	}
	queue := make(chan task, 128)
	go func() {
		defer c.releaseDocuments()
		initialized, shutdown := false, false
		for {
			select {
			case <-peer.ctx.Done():
				return
			case job := <-queue:
				request := job.ahpRequest
				ctx, done := job.ctx, job.done
				if request.Method == "initialize" {
					done()
					if initialized {
						peer.reply(request.ID, nil, ahpInvalid("already initialized"))
						continue
					}
					initialized = true
					peer.reply(request.ID, map[string]any{"serverInfo": map[string]any{"name": "Wingman"}, "capabilities": c.capabilities()}, nil)
					continue
				}
				if request.Method == "exit" {
					peer.cancel()
					return
				}
				if !initialized || shutdown {
					done()
					peer.reply(request.ID, nil, &ahpRPCError{Code: -32002, Message: "language connection is not initialized"})
					continue
				}
				if request.Method == "initialized" {
					continue
				}
				if request.Method == "shutdown" {
					done()
					shutdown = true
					peer.reply(request.ID, nil, nil)
					continue
				}
				if len(request.ID) == 0 {
					if err := c.documentNotification(request.Method, request.Params); err != nil {
						peer.notify("window/logMessage", map[string]any{"type": 1, "message": err.Error()})
					}
					continue
				}
				if err := ctx.Err(); err != nil {
					done()
					peer.reply(request.ID, nil, err)
					continue
				}
				// Capture buffers before the next notification is processed.
				c.mu.Lock()
				buffers := make(map[string]lspBuffer, len(c.documents))
				for uri, doc := range c.documents {
					buffers[uri] = doc
				}
				c.mu.Unlock()
				go func() {
					defer done()
					result, err := c.request(ctx, request, buffers)
					if err == nil && result != nil {
						var data []byte
						data, err = lsp.Marshal(result)
						result = json.RawMessage(data)
					}
					peer.reply(request.ID, result, err)
				}()
			}
		}
	}()
	for {
		_, data, err := peer.writer.conn.Read(peer.ctx)
		if err != nil {
			return
		}
		var request ahpRequest
		if json.Unmarshal(data, &request) != nil || request.JSONRPC != "2.0" {
			peer.reply(json.RawMessage("null"), nil, &ahpRPCError{Code: -32600, Message: "invalid JSON-RPC message"})
			continue
		}
		if request.Method == "" {
			c.mu.Lock()
			ch := c.replies[string(request.ID)]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- data:
				default:
				}
			}
			continue
		}
		if request.Method == "$/cancelRequest" {
			var p struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(request.Params, &p)
			peer.cancelRequest(string(p.ID))
			continue
		}
		job := task{ahpRequest: request, ctx: peer.ctx, done: func() {}}
		if len(request.ID) > 0 {
			ctx, done, err := peer.begin(string(request.ID))
			if err != nil {
				peer.reply(request.ID, nil, err)
				continue
			}
			job.ctx, job.done = ctx, done
		}
		select {
		case queue <- job:
		case <-peer.ctx.Done():
			return
		default:
			peer.cancel()
			return
		}
	}
}

func (c *lspConnection) capabilities() map[string]any {
	return map[string]any{
		"positionEncoding": "utf-16", "textDocumentSync": map[string]any{"openClose": true, "change": 1, "save": map[string]any{"includeText": true}},
		"completionProvider": map[string]any{"resolveProvider": true, "triggerCharacters": []string{".", ":", "/"}},
		"hoverProvider":      true, "definitionProvider": true, "typeDefinitionProvider": true, "implementationProvider": true, "referencesProvider": true,
		"documentSymbolProvider": true, "documentHighlightProvider": true, "foldingRangeProvider": true,
		"signatureHelpProvider": map[string]any{"triggerCharacters": []string{"(", ","}}, "renameProvider": map[string]any{"prepareProvider": true},
		"codeActionProvider": map[string]any{"resolveProvider": true}, "documentFormattingProvider": true, "documentRangeFormattingProvider": true,
		"documentOnTypeFormattingProvider": map[string]any{"firstTriggerCharacter": "\n", "moreTriggerCharacter": []string{";", "}"}},
		"semanticTokensProvider":           map[string]any{"legend": map[string]any{"tokenTypes": lspTokenTypes, "tokenModifiers": lspTokenModifiers}, "full": true},
		"inlayHintProvider":                true, "diagnosticProvider": map[string]any{"interFileDependencies": true, "workspaceDiagnostics": true},
		"executeCommandProvider": map[string]any{"commands": []string{}},
		"experimental":           map[string]any{"wingman": map[string]any{"workspaceURI": fileuri.FromPath(c.server.workspace.RootPath), "scope": c.server.scope, "documentCapabilities": true}},
	}
}

func (c *lspConnection) path(uri string) (string, error) {
	p, ok := fileuri.Path(uri)
	if !ok {
		return "", ahpInvalid("expected a file URI")
	}
	rel, err := filepath.Rel(c.server.workspace.RootPath, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", ahpInvalid("document is outside the workspace")
	}
	// os.Root also rejects symlinks escaping the workspace.
	info, err := c.server.workspace.Root.Stat(rel)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", ahpInvalid("document is not a regular file")
	}
	return filepath.Join(c.server.workspace.RootPath, rel), nil
}

func (c *lspConnection) documentNotification(method string, raw json.RawMessage) error {
	if !strings.HasPrefix(method, "textDocument/did") {
		return nil
	}
	var p struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Text    string `json:"text"`
			Version int    `json:"version"`
		} `json:"textDocument"`
		Text           *string `json:"text"`
		ContentChanges []struct {
			Text  string          `json:"text"`
			Range json.RawMessage `json:"range"`
		} `json:"contentChanges"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ahpInvalid("invalid document notification")
	}
	uri := p.TextDocument.URI
	if method == "textDocument/didClose" {
		c.mu.Lock()
		defer c.mu.Unlock()
		if doc, exists := c.documents[uri]; exists {
			delete(c.documents, uri)
			// The file may have been deleted or renamed since didOpen.
			return c.releaseDocument(doc.path)
		}
		return nil
	}
	path, err := c.path(uri)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	doc, exists := c.documents[uri]
	switch method {
	case "textDocument/didOpen":
		if exists {
			return ahpInvalid("document already open")
		}
		doc = lspBuffer{path: path, text: p.TextDocument.Text, version: p.TextDocument.Version}
	case "textDocument/didChange":
		if !exists || p.TextDocument.Version <= doc.version {
			return ahpInvalid("document version must increase")
		}
		for _, change := range p.ContentChanges {
			if len(change.Range) != 0 && string(change.Range) != "null" {
				return ahpInvalid("server negotiated full document synchronization")
			}
			doc.text = change.Text
		}
		doc.version = p.TextDocument.Version
	case "textDocument/didSave":
		if !exists {
			return ahpInvalid("document is not open")
		}
		if p.Text != nil {
			doc.text = *p.Text
		}
	default:
		return nil
	}
	c.documents[uri] = doc
	s := c.server
	s.lspDocumentsMu.Lock()
	defer s.lspDocumentsMu.Unlock()
	if s.lspDocuments == nil {
		s.lspDocuments = map[string]map[*lspConnection]struct{}{}
	}
	if s.lspDocuments[path] == nil {
		s.lspDocuments[path] = map[*lspConnection]struct{}{}
	}
	s.lspDocuments[path][c] = struct{}{}
	return c.server.workspace.SyncEditorDocument(c.ctx, path, doc.text, method == "textDocument/didSave")
}

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

func (c *lspConnection) request(ctx context.Context, req ahpRequest, buffers map[string]lspBuffer) (any, error) {
	s, w := c.server, c.server.workspace
	if req.Method == "workspace/diagnostic" {
		report := w.Diagnostics(ctx)
		items := []any{}
		for path, diagnostics := range report.Diagnostics {
			items = append(items, map[string]any{"uri": fileuri.FromPath(path), "version": nil, "kind": "full", "items": diagnostics})
		}
		return map[string]any{"items": items, "_wingman": map[string]any{"checked_files": report.CheckedFiles, "discovered_files": report.DiscoveredFiles, "discovery_truncated": report.DiscoveryTruncated, "unknown_files": report.UnknownFiles, "unavailable_servers": report.UnavailableServers, "analyzing": report.Analyzing}}, nil
	}
	if req.Method == "wingman/activity" {
		return s.lspActivity(), nil
	}
	switch req.Method {
	case "wingman/documentCapabilities", "textDocument/diagnostic", "textDocument/definition", "textDocument/typeDefinition", "textDocument/implementation", "textDocument/references", "textDocument/hover", "textDocument/completion", "completionItem/resolve", "textDocument/signatureHelp", "textDocument/documentSymbol", "textDocument/documentHighlight", "textDocument/foldingRange", "textDocument/semanticTokens/full", "textDocument/prepareRename", "textDocument/rename", "textDocument/codeAction", "codeAction/resolve", "workspace/executeCommand", "textDocument/formatting", "textDocument/rangeFormatting", "textDocument/onTypeFormatting", "textDocument/inlayHint":
	default:
		return nil, &ahpRPCError{Code: -32601, Message: "unsupported language method: " + req.Method}
	}
	var p lspFeatureParams
	if err := lsp.Unmarshal(req.Params, &p); err != nil {
		return nil, ahpInvalid("invalid language request")
	}
	uri := p.TextDocument.URI
	// Resolve data is opaque to clients. Wrap rather than overwrite the
	// downstream language server's own data and recover its document route.
	if req.Method == "completionItem/resolve" || req.Method == "codeAction/resolve" {
		var route struct {
			URI  string          `json:"uri"`
			Data json.RawMessage `json:"data"`
		}
		var data map[string]json.RawMessage
		if json.Unmarshal(p.Data, &data) != nil || json.Unmarshal(data["wingman"], &route) != nil || route.URI == "" {
			return nil, ahpInvalid("missing resolve routing data")
		}
		uri = route.URI
		var item map[string]json.RawMessage
		_ = json.Unmarshal(req.Params, &item)
		if len(route.Data) == 0 {
			delete(item, "data")
		} else {
			item["data"] = route.Data
		}
		req.Params, _ = json.Marshal(item)
	}
	path, err := c.path(uri)
	if err != nil {
		return nil, err
	}
	var content *string
	if doc, ok := buffers[uri]; ok {
		content = &doc.text
	}
	line, column := p.Position.Line, p.Position.Character
	if line < 0 || column < 0 {
		return nil, ahpInvalid("negative document position")
	}
	switch req.Method {
	case "wingman/documentCapabilities":
		return s.lspEditorCapabilities(ctx, path), nil
	case "textDocument/diagnostic":
		diagnostics, known, err := w.FileDiagnostics(ctx, path, content)
		if err != nil {
			return nil, err
		}
		if !known {
			return nil, &ahpRPCError{Code: -32802, Message: "diagnostics are not available yet", Data: map[string]any{"retriggerRequest": true}}
		}
		if diagnostics == nil {
			diagnostics = []language.Diagnostic{}
		}
		return map[string]any{"kind": "full", "items": diagnostics}, nil
	case "textDocument/definition", "textDocument/typeDefinition", "textDocument/implementation", "textDocument/references":
		methods := map[string]locationRequest{"textDocument/definition": w.DefinitionLocations, "textDocument/typeDefinition": w.TypeDefinitionLocations, "textDocument/implementation": w.ImplementationLocations, "textDocument/references": w.ReferenceLocations}
		locations, err := methods[req.Method](ctx, path, content, line, column)
		for _, loc := range locations {
			if target, ok := fileuri.Path(string(loc.URI)); ok {
				if rel, err := filepath.Rel(w.RootPath, target); err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
					if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
						s.allowLSPExternalFile(filepath.ToSlash(target))
					}
				}
			}
		}
		if locations == nil {
			locations = []lsp.Location{}
		}
		return locations, err
	case "textDocument/hover":
		text, err := w.HoverInformation(ctx, path, content, line, column)
		return map[string]any{"contents": map[string]string{"kind": "markdown", "value": text}}, err
	case "textDocument/completion":
		var trigger *lsp.CompletionContext
		if len(p.Context) > 0 {
			if err := lsp.Unmarshal(p.Context, &trigger); err != nil {
				return nil, err
			}
		}
		list, err := w.CompletionItems(ctx, path, content, line, column, trigger)
		if err != nil {
			return nil, err
		}
		if list.Items == nil {
			list.Items = []lsp.CompletionItem{}
		}
		return routeLSPItems(list, uri, "items")
	case "completionItem/resolve":
		var item lsp.CompletionItem
		if err := lsp.Unmarshal(req.Params, &item); err != nil {
			return nil, err
		}
		return w.ResolveCompletionItem(ctx, path, content, item)
	case "textDocument/signatureHelp":
		var trigger *lsp.SignatureHelpContext
		if len(p.Context) > 0 {
			if err := lsp.Unmarshal(p.Context, &trigger); err != nil {
				return nil, err
			}
		}
		return w.SignatureHelp(ctx, path, content, line, column, trigger)
	case "textDocument/documentSymbol":
		documents, flat, err := w.DocumentSymbolItems(ctx, path, content)
		if len(documents) > 0 {
			return documents, err
		}
		converted := []lsp.DocumentSymbol{}
		for _, symbol := range flat {
			converted = append(converted, lsp.DocumentSymbol{Name: symbol.Name, Kind: symbol.Kind, Range: symbol.Location.Range, SelectionRange: symbol.Location.Range})
		}
		return converted, err
	case "textDocument/documentHighlight":
		return w.DocumentHighlights(ctx, path, content, line, column)
	case "textDocument/foldingRange":
		return w.FoldingRanges(ctx, path, content)
	case "textDocument/semanticTokens/full":
		tokens, err := w.SemanticTokens(ctx, path, content)
		if err != nil {
			return nil, err
		}
		return encodeLSPSemanticTokens(tokens), nil
	case "textDocument/prepareRename":
		return w.PrepareRename(ctx, path, content, line, column)
	case "textDocument/rename":
		if strings.TrimSpace(p.NewName) == "" {
			return nil, ahpInvalid("newName is required")
		}
		edit, err := w.Rename(ctx, path, content, line, column, p.NewName)
		if err != nil {
			return nil, err
		}
		return s.lspEdit(edit)
	case "textDocument/codeAction":
		var actionContext struct {
			Only        []lsp.CodeActionKind      `json:"only"`
			TriggerKind lsp.CodeActionTriggerKind `json:"triggerKind"`
		}
		if len(p.Context) > 0 {
			if err := json.Unmarshal(p.Context, &actionContext); err != nil {
				return nil, err
			}
		}
		actions, err := w.CodeActions(ctx, path, content, p.Range, actionContext.Only, actionContext.TriggerKind)
		if err != nil {
			return nil, err
		}
		if actions == nil {
			actions = []lsp.CommandOrCodeAction{}
		}
		result, err := routeLSPItems(actions, uri, "")
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
				action["edit"], err = s.lspEdit(&parsed)
				if err != nil {
					return nil, err
				}
			}
		}
		return result, nil
	case "codeAction/resolve":
		var action lsp.CodeAction
		if err := lsp.Unmarshal(req.Params, &action); err != nil {
			return nil, err
		}
		resolved, err := w.ResolveCodeAction(ctx, path, content, action)
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
			result["edit"], err = s.lspEdit(resolved.Edit)
		}
		return result, err
	case "workspace/executeCommand":
		if p.Command == "" {
			return nil, ahpInvalid("command is required")
		}
		result, edits, err := w.ExecuteLSPCommandWithEdits(ctx, path, content, lsp.Command{Command: p.Command, Arguments: p.Arguments})
		if err != nil {
			return nil, err
		}
		for _, requested := range edits {
			edit, err := s.lspEdit(&requested.Edit)
			if err != nil {
				return nil, err
			}
			if err := c.applyEdit(ctx, req.ID, requested.Label, edit); err != nil {
				return nil, err
			}
		}
		return result, nil
	case "textDocument/formatting":
		return w.Formatting(ctx, path, content, p.Options)
	case "textDocument/rangeFormatting":
		return w.RangeFormatting(ctx, path, content, p.Range, p.Options)
	case "textDocument/onTypeFormatting":
		return w.OnTypeFormatting(ctx, path, content, line, column, p.Character, p.Options)
	case "textDocument/inlayHint":
		return w.InlayHints(ctx, path, content, p.Range)
	default:
		return nil, &ahpRPCError{Code: -32601, Message: "unsupported language method: " + req.Method}
	}
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

func (s *Server) lspEdit(edit *lsp.WorkspaceEdit) (any, error) {
	if edit == nil {
		return nil, nil
	}
	documents, err := s.workspaceEditDocuments(edit)
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
	result["_wingman"] = map[string]any{"documents": documents}
	return result, nil
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

func (c *lspConnection) applyEdit(ctx context.Context, origin json.RawMessage, label string, edit any) error {
	id := fmt.Sprintf("apply:%d", c.sequence.Add(1))
	key, _ := json.Marshal(id)
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	c.replies[string(key)] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.replies, string(key)); c.mu.Unlock() }()
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "workspace/applyEdit", "params": map[string]any{"label": label, "edit": edit, "_wingmanRequestId": origin}})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case wire := <-ch:
		var response struct {
			Result struct {
				Applied       bool   `json:"applied"`
				FailureReason string `json:"failureReason"`
			} `json:"result"`
		}
		if err := json.Unmarshal(wire, &response); err != nil {
			return err
		}
		if !response.Result.Applied {
			return errors.New("workspace edit was not applied: " + response.Result.FailureReason)
		}
		return nil
	}
}

func (c *lspConnection) releaseDocument(path string) error {
	s := c.server
	s.lspDocumentsMu.Lock()
	defer s.lspDocumentsMu.Unlock()
	owners := s.lspDocuments[path]
	delete(owners, c)
	if len(owners) != 0 {
		return nil
	}
	delete(s.lspDocuments, path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.workspace.CloseEditorDocument(ctx, path)
}
func (c *lspConnection) releaseDocuments() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, doc := range c.documents {
		_ = c.releaseDocument(doc.path)
	}
	clear(c.documents)
}
