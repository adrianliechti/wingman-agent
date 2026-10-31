package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
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
	reported  map[string]bool // documents in the last workspace diagnostic report
	replies   map[string]chan json.RawMessage
	sequence  atomic.Uint64
}

func validLSPID(id json.RawMessage) bool {
	return len(id) == 0 || id[0] == '"' || id[0] == '-' || id[0] >= '0' && id[0] <= '9'
}

func (s *Server) handleLSP(w http.ResponseWriter, r *http.Request) {
	peer, err := s.acceptEditorSocket(w, r)
	if err != nil {
		return
	}
	defer peer.close()
	c := &lspConnection{editorSocket: peer, server: s, documents: map[string]lspBuffer{}, replies: map[string]chan json.RawMessage{}}
	defer func() {
		s.lspDocumentsMu.Lock()
		delete(s.lspRefreshers, c)
		s.lspDocumentsMu.Unlock()
	}()
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
						peer.reply(request.ID, nil, &ahpRPCError{Code: -32600, Message: "already initialized"})
						continue
					}
					initialized = true
					var params struct {
						Capabilities struct {
							Workspace struct {
								Diagnostics struct {
									RefreshSupport bool `json:"refreshSupport"`
								} `json:"diagnostics"`
							} `json:"workspace"`
						} `json:"capabilities"`
					}
					if json.Unmarshal(request.Params, &params) == nil && params.Capabilities.Workspace.Diagnostics.RefreshSupport {
						s.lspDocumentsMu.Lock()
						if s.lspRefreshers == nil {
							s.lspRefreshers = map[*lspConnection]struct{}{}
						}
						s.lspRefreshers[c] = struct{}{}
						s.lspDocumentsMu.Unlock()
					}
					peer.reply(request.ID, map[string]any{"serverInfo": map[string]any{"name": "Wingman"}, "capabilities": c.capabilities()}, nil)
					continue
				}
				if request.Method == "exit" {
					peer.cancel()
					return
				}
				if !initialized {
					done()
					peer.reply(request.ID, nil, &ahpRPCError{Code: -32002, Message: "language connection is not initialized"})
					continue
				}
				if shutdown {
					done()
					peer.reply(request.ID, nil, &ahpRPCError{Code: -32600, Message: "language connection is shut down"})
					continue
				}
				if request.Method == "initialized" {
					done()
					peer.reply(request.ID, nil, &ahpRPCError{Code: -32600, Message: "initialized is a notification"})
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
					peer.reply(request.ID, result, editorRequestError(ctx, err))
				}()
			}
		}
	}()
	for {
		_, data, err := peer.writer.conn.Read(peer.ctx)
		if err != nil {
			return
		}
		if !json.Valid(data) {
			peer.reply(json.RawMessage("null"), nil, &ahpRPCError{Code: -32700, Message: "parse error"})
			continue
		}
		var request ahpRequest
		if json.Unmarshal(data, &request) != nil || request.JSONRPC != "2.0" || !validLSPID(request.ID) || request.Method == "" && len(request.ID) == 0 {
			id := json.RawMessage("null")
			if len(request.ID) > 0 && validLSPID(request.ID) {
				id = request.ID
			}
			peer.reply(id, nil, &ahpRPCError{Code: -32600, Message: "invalid JSON-RPC message"})
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
			timeout := 2 * time.Minute
			if request.Method == "workspace/executeCommand" {
				timeout = 0 // command edits wait for the user's review; the native call stays bounded
			}
			ctx, done, err := peer.begin(string(request.ID), timeout)
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
		"workspace":              map[string]any{"textDocumentContent": map[string]any{"schemes": []string{lspSourceScheme}}},
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
	if errors.Is(err, fs.ErrNotExist) {
		// Editors may open a buffer before its file is first saved.
		if parent, statErr := c.server.workspace.Root.Stat(filepath.Dir(rel)); statErr == nil && parent.IsDir() {
			return filepath.Join(c.server.workspace.RootPath, rel), nil
		}
	}
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
		doc, exists := c.documents[uri]
		delete(c.documents, uri)
		c.mu.Unlock()
		if exists {
			// The file may have been deleted or renamed since didOpen.
			return c.releaseDocument(doc.path)
		}
		return nil
	}
	path, err := c.path(uri)
	if err != nil {
		return err
	}
	saved := method == "textDocument/didSave"
	if method == "textDocument/didOpen" {
		// A buffer that matches the file lets disk-backed agent reads refresh it.
		disk, readErr := os.ReadFile(path)
		saved = readErr == nil && string(disk) == p.TextDocument.Text
	}
	c.mu.Lock()
	doc, exists := c.documents[uri]
	switch method {
	case "textDocument/didOpen":
		if exists {
			c.mu.Unlock()
			return ahpInvalid("document already open")
		}
		doc = lspBuffer{path: path, text: p.TextDocument.Text, version: p.TextDocument.Version}
	case "textDocument/didChange":
		if !exists || p.TextDocument.Version <= doc.version {
			c.mu.Unlock()
			return ahpInvalid("document version must increase")
		}
		for _, change := range p.ContentChanges {
			if len(change.Range) != 0 && string(change.Range) != "null" {
				c.mu.Unlock()
				return ahpInvalid("server negotiated full document synchronization")
			}
			doc.text = change.Text
		}
		doc.version = p.TextDocument.Version
	case "textDocument/didSave":
		if !exists {
			c.mu.Unlock()
			return ahpInvalid("document is not open")
		}
		if p.Text != nil {
			doc.text = *p.Text
		}
	default:
		c.mu.Unlock()
		return nil
	}
	c.documents[uri] = doc
	c.mu.Unlock()
	s := c.server
	s.lspDocumentsMu.Lock()
	if s.lspDocuments == nil {
		s.lspDocuments = map[string]map[*lspConnection]struct{}{}
	}
	if s.lspDocuments[path] == nil {
		s.lspDocuments[path] = map[*lspConnection]struct{}{}
	}
	s.lspDocuments[path][c] = struct{}{}
	s.lspDocumentsMu.Unlock()
	// This connection's queue keeps updates ordered while a cold language
	// server starts; no gateway lock is held across that wait.
	return s.workspace.SyncEditorDocument(c.ctx, path, doc.text, saved)
}

func (c *lspConnection) applyEdit(ctx context.Context, origin json.RawMessage, label string, edit any) (bool, error) {
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
		return false, ctx.Err()
	case wire := <-ch:
		var response struct {
			Result struct {
				Applied bool `json:"applied"`
			} `json:"result"`
			Error *ahpRPCError `json:"error"`
		}
		if err := json.Unmarshal(wire, &response); err != nil {
			return false, err
		}
		if response.Error != nil {
			return false, response.Error
		}
		return response.Result.Applied, nil
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

// refreshDiagnostics asks clients to pull diagnostics again (LSP
// workspace/diagnostic/refresh). Their responses carry nothing to wait for.
func (s *Server) refreshDiagnostics() {
	s.lspDocumentsMu.Lock()
	clients := make([]*lspConnection, 0, len(s.lspRefreshers))
	for c := range s.lspRefreshers {
		clients = append(clients, c)
	}
	s.lspDocumentsMu.Unlock()
	for _, c := range clients {
		c.send(map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("refresh:%d", c.sequence.Add(1)), "method": "workspace/diagnostic/refresh"})
	}
}
