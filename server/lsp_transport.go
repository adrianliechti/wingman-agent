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
	saved   bool
}

// lspDocument is the language server's single copy of a file that several
// connections may have open; mu orders its synchronization.
type lspDocument struct {
	mu      sync.Mutex
	owners  map[*lspConnection]struct{} // guarded by Server.lspDocumentsMu
	writer  *lspConnection              // whose buffer the language server has
	changes uint64                      // counts writer switches
}

// lspQueueLimit bounds the bytes waiting behind a slow document notification.
const lspQueueLimit = 128 << 20

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
	queue := &lspQueue{ready: make(chan struct{}, 1)}
	go func() {
		defer c.releaseDocuments()
		initialized, shutdown := false, false
		for {
			job, ok := queue.pop(peer.ctx)
			if !ok {
				return
			}
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
				peer.reply(request.ID, nil, editorRequestError(ctx, err))
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
		var message struct {
			ahpRequest
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		decodeErr := json.Unmarshal(data, &message)
		request := message.ahpRequest
		if decodeErr == nil && request.Method == "" && (len(request.ID) > 0 || message.Result != nil || message.Error != nil) {
			// Responses, even malformed ones, are never answered.
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
		if decodeErr != nil || request.JSONRPC != "2.0" || !validLSPID(request.ID) || request.Method == "" {
			id := json.RawMessage("null")
			if len(request.ID) > 0 && validLSPID(request.ID) {
				id = request.ID
			}
			peer.reply(id, nil, &ahpRPCError{Code: -32600, Message: "invalid JSON-RPC message"})
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
		job := lspTask{ahpRequest: request, ctx: peer.ctx, done: func() {}, size: len(data)}
		if len(request.ID) == 0 && request.Method == "textDocument/didChange" {
			var p struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
				} `json:"textDocument"`
			}
			if json.Unmarshal(request.Params, &p) == nil {
				job.change, job.version = p.TextDocument.URI, p.TextDocument.Version
			}
		}
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
		if !queue.push(job) {
			peer.cancel()
			return
		}
	}
}

type lspTask struct {
	ahpRequest
	ctx     context.Context
	done    func()
	size    int
	change  string // document of a didChange notification
	version int
}

// lspQueue holds messages waiting behind a document notification. Consecutive
// full-text changes to one document collapse into the newest, so typing while
// a language server starts does not grow it.
type lspQueue struct {
	mu    sync.Mutex
	jobs  []lspTask
	bytes int
	ready chan struct{}
}

func (q *lspQueue) push(job lspTask) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n := len(q.jobs); n > 0 && job.change != "" && q.jobs[n-1].change == job.change && job.version > q.jobs[n-1].version {
		q.bytes -= q.jobs[n-1].size
		q.jobs[n-1] = job
	} else {
		q.jobs = append(q.jobs, job)
	}
	q.bytes += job.size
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return q.bytes <= lspQueueLimit
}

func (q *lspQueue) pop(ctx context.Context) (lspTask, bool) {
	for {
		q.mu.Lock()
		if len(q.jobs) > 0 && ctx.Err() == nil {
			job := q.jobs[0]
			q.jobs[0] = lspTask{}
			q.jobs = q.jobs[1:]
			q.bytes -= job.size
			q.mu.Unlock()
			return job, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return lspTask{}, false
		case <-q.ready:
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
		_, aliased := c.buffer(doc.path)
		c.mu.Unlock()
		if exists && !aliased {
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
	doc.saved = saved
	c.documents[uri] = doc
	c.mu.Unlock()
	s := c.server
	s.lspDocumentsMu.Lock()
	if s.lspDocuments == nil {
		s.lspDocuments = map[string]*lspDocument{}
	}
	shared := s.lspDocuments[path]
	if shared == nil {
		shared = &lspDocument{owners: map[*lspConnection]struct{}{}}
		s.lspDocuments[path] = shared
	}
	shared.owners[c] = struct{}{}
	s.lspDocumentsMu.Unlock()
	// This connection's queue keeps updates ordered while a cold language
	// server starts; only this document waits.
	shared.mu.Lock()
	defer shared.mu.Unlock()
	return c.syncShared(c.ctx, shared, path)
}

// buffer finds this connection's buffer of a file; c.mu must be held.
func (c *lspConnection) buffer(path string) (lspBuffer, bool) {
	for _, doc := range c.documents {
		if doc.path == path {
			return doc, true
		}
	}
	return lspBuffer{}, false
}

// syncShared gives the language server this connection's current buffer of
// path; shared.mu must be held.
func (c *lspConnection) syncShared(ctx context.Context, shared *lspDocument, path string) error {
	c.mu.Lock()
	doc, ok := c.buffer(path)
	c.mu.Unlock()
	if !ok {
		return nil
	}
	if shared.writer != c {
		shared.writer = c
		shared.changes++
	}
	if err := c.server.workspace.SyncEditorDocument(ctx, path, doc.text, doc.saved); err != nil {
		shared.writer = nil
		return err
	}
	return nil
}

// claimDocument makes the language server use this connection's buffer of path
// for a request. The returned check reports whether it still does.
func (c *lspConnection) claimDocument(ctx context.Context, path string) (func() bool, error) {
	s := c.server
	s.lspDocumentsMu.Lock()
	shared := s.lspDocuments[path]
	s.lspDocumentsMu.Unlock()
	if shared == nil {
		return nil, nil
	}
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.writer != c {
		if err := c.syncShared(ctx, shared, path); err != nil {
			return nil, err
		}
	}
	changes := shared.changes
	return func() bool {
		shared.mu.Lock()
		defer shared.mu.Unlock()
		return shared.changes == changes
	}, nil
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

// releaseDocument closes the language server's document when its last owner
// leaves, or restores a remaining owner's buffer in place of this one.
func (c *lspConnection) releaseDocument(path string) error {
	s := c.server
	s.lspDocumentsMu.Lock()
	shared := s.lspDocuments[path]
	s.lspDocumentsMu.Unlock()
	if shared == nil {
		return nil
	}
	shared.mu.Lock()
	defer shared.mu.Unlock()
	s.lspDocumentsMu.Lock()
	delete(shared.owners, c)
	_, current := shared.owners[shared.writer]
	owners := make([]*lspConnection, 0, len(shared.owners))
	for owner := range shared.owners {
		owners = append(owners, owner)
	}
	s.lspDocumentsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if len(owners) > 0 {
		for _, owner := range owners {
			if current {
				break
			}
			if err := owner.syncShared(ctx, shared, path); err != nil {
				return err
			}
			current = shared.writer == owner
		}
		return nil
	}
	shared.writer = nil
	err := s.workspace.CloseEditorDocument(ctx, path)
	// The entry stays registered during the close so a new owner waits for it.
	s.lspDocumentsMu.Lock()
	if len(shared.owners) == 0 && s.lspDocuments[path] == shared {
		delete(s.lspDocuments, path)
	}
	s.lspDocumentsMu.Unlock()
	return err
}

func (c *lspConnection) releaseDocuments() {
	c.mu.Lock()
	paths := map[string]bool{}
	for _, doc := range c.documents {
		paths[doc.path] = true
	}
	clear(c.documents)
	c.mu.Unlock()
	for path := range paths {
		_ = c.releaseDocument(path)
	}
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
