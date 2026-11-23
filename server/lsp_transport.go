package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/server/ahp"
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
	owners  map[*lspConnection]struct{} // guarded by editorState.lspDocumentsMu
	writer  *lspConnection              // whose buffer the language server has
	buffer  lspBuffer
	changes uint64 // counts changes of writer or buffer
}

// lspQueueLimit bounds the bytes waiting behind a slow document notification.
const lspQueueLimit = 128 << 20

func validLSPID(id json.RawMessage) bool {
	return len(id) == 0 || id[0] == '"' || id[0] == '-' || id[0] >= '0' && id[0] <= '9'
}

func (s *Server) handleLSP(w http.ResponseWriter, r *http.Request) {
	c, err := s.acceptLSPConnection(w, r)
	if err != nil {
		return
	}
	defer c.close()
	// Document notifications are processed in receive order. Requests begin only
	// after preceding changes have been applied; cancellation never queues behind
	// a slow language server.
	queue := &lspQueue{ready: make(chan struct{}, 1)}
	go func() {
		defer c.releaseDocuments()
		initialized, shutdown := false, false
		for {
			job, ok := queue.pop(c.ctx)
			if !ok {
				return
			}
			request := job.Request
			ctx, done := job.ctx, job.done
			if request.Method == "initialize" {
				done()
				if initialized {
					c.reply(request.ID, nil, &ahp.Error{Code: -32600, Message: "already initialized"})
					continue
				}
				initialized = true
				c.reply(request.ID, map[string]any{"serverInfo": map[string]any{"name": "Wingman"}, "capabilities": c.capabilities()}, nil)
				continue
			}
			if request.Method == "exit" {
				c.cancel()
				return
			}
			if !initialized {
				done()
				c.reply(request.ID, nil, &ahp.Error{Code: -32002, Message: "language connection is not initialized"})
				continue
			}
			if shutdown {
				done()
				c.reply(request.ID, nil, &ahp.Error{Code: -32600, Message: "language connection is shut down"})
				continue
			}
			if request.Method == "initialized" {
				done()
				c.reply(request.ID, nil, &ahp.Error{Code: -32600, Message: "initialized is a notification"})
				continue
			}
			if request.Method == "shutdown" {
				done()
				shutdown = true
				c.reply(request.ID, nil, nil)
				continue
			}
			if len(request.ID) == 0 {
				if err := c.documentNotification(request.Method, request.Params); err != nil {
					c.notify("window/logMessage", map[string]any{"type": 1, "message": err.Error()})
				}
				continue
			}
			if err := ctx.Err(); err != nil {
				done()
				c.reply(request.ID, nil, editorRequestError(ctx, err))
				continue
			}
			// Capture buffers before the next notification is processed.
			c.mu.Lock()
			buffers := maps.Clone(c.documents)
			c.mu.Unlock()
			go func() {
				defer done()
				result, err := c.request(ctx, request, buffers)
				if err == nil && result != nil {
					var data []byte
					data, err = lsp.Marshal(result)
					result = json.RawMessage(data)
				}
				c.reply(request.ID, result, editorRequestError(ctx, err))
			}()
		}
	}()
	for {
		_, data, err := c.writer.conn.Read(c.ctx)
		if err != nil {
			return
		}
		if !json.Valid(data) {
			c.reply(json.RawMessage("null"), nil, &ahp.Error{Code: -32700, Message: "parse error"})
			continue
		}
		var message struct {
			ahp.Request
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		decodeErr := json.Unmarshal(data, &message)
		request := message.Request
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
			c.reply(id, nil, &ahp.Error{Code: -32600, Message: "invalid JSON-RPC message"})
			continue
		}
		if request.Method == "$/cancelRequest" {
			var p struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(request.Params, &p)
			c.cancelRequest(string(p.ID))
			continue
		}
		job := lspTask{Request: request, ctx: c.ctx, done: func() {}, size: len(data)}
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
			ctx, done, err := c.begin(string(request.ID), timeout)
			if err != nil {
				c.reply(request.ID, nil, err)
				continue
			}
			job.ctx, job.done = ctx, done
		}
		if !queue.push(job) {
			c.cancel()
			return
		}
	}
}

type lspTask struct {
	ahp.Request
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
		"inlayHintProvider":                true, "diagnosticProvider": map[string]any{"interFileDependencies": true, "workspaceDiagnostics": false},
		"executeCommandProvider": map[string]any{"commands": []string{}},
		"workspace":              map[string]any{"textDocumentContent": map[string]any{"schemes": []string{lspSourceScheme}}},
		"experimental":           map[string]any{"wingman": map[string]any{"workspaceURI": fileuri.FromPath(c.server.Project.workspace.RootPath), "scope": c.server.Project.scope, "documentCapabilities": true}},
	}
}

func (c *lspConnection) path(uri string) (string, error) {
	p, ok := fileuri.Path(uri)
	if !ok {
		return "", ahp.InvalidParams("expected a file URI")
	}
	rel, err := filepath.Rel(c.server.Project.workspace.RootPath, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", ahp.InvalidParams("document is outside the workspace")
	}
	// os.Root also rejects symlinks escaping the workspace.
	info, err := c.server.Project.workspace.Root.Stat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		// Editors may open a buffer before its file is first saved.
		if parent, statErr := c.server.Project.workspace.Root.Stat(filepath.Dir(rel)); statErr == nil && parent.IsDir() {
			return filepath.Join(c.server.Project.workspace.RootPath, rel), nil
		}
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", ahp.InvalidParams("document is not a regular file")
	}
	return filepath.Join(c.server.Project.workspace.RootPath, rel), nil
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
		return ahp.InvalidParams("invalid document notification")
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
			return ahp.InvalidParams("document already open")
		}
		doc = lspBuffer{path: path, text: p.TextDocument.Text, version: p.TextDocument.Version}
	case "textDocument/didChange":
		if !exists || p.TextDocument.Version <= doc.version {
			c.mu.Unlock()
			return ahp.InvalidParams("document version must increase")
		}
		for _, change := range p.ContentChanges {
			if len(change.Range) != 0 && string(change.Range) != "null" {
				c.mu.Unlock()
				return ahp.InvalidParams("server negotiated full document synchronization")
			}
			doc.text = change.Text
		}
		doc.version = p.TextDocument.Version
	case "textDocument/didSave":
		if !exists {
			c.mu.Unlock()
			return ahp.InvalidParams("document is not open")
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
	s.editor.lspDocumentsMu.Lock()
	if s.editor.lspDocuments == nil {
		s.editor.lspDocuments = map[string]*lspDocument{}
	}
	shared := s.editor.lspDocuments[path]
	if shared == nil {
		shared = &lspDocument{owners: map[*lspConnection]struct{}{}}
		s.editor.lspDocuments[path] = shared
	}
	shared.owners[c] = struct{}{}
	s.editor.lspDocumentsMu.Unlock()
	// This connection's queue keeps updates ordered while a cold language
	// server starts; only this document waits.
	shared.mu.Lock()
	defer shared.mu.Unlock()
	return c.syncShared(c.ctx, shared, doc)
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

// syncShared sends the buffer to the language server with shared.mu held.
func (c *lspConnection) syncShared(ctx context.Context, shared *lspDocument, doc lspBuffer) error {
	if shared.writer != c || shared.buffer != doc {
		shared.changes++
	}
	if err := c.language.SyncDocument(ctx, doc.path, doc.text, doc.saved); err != nil {
		shared.writer = nil
		return err
	}
	shared.writer, shared.buffer = c, doc
	return nil
}

// claimDocument selects the request's snapshot. Its check detects changes even
// if another editor claimed the document and the original editor took it back.
func (c *lspConnection) claimDocument(ctx context.Context, uri string, doc lspBuffer) (func() bool, error) {
	s := c.server
	s.editor.lspDocumentsMu.Lock()
	shared := s.editor.lspDocuments[doc.path]
	s.editor.lspDocumentsMu.Unlock()
	if shared == nil {
		return nil, errLSPContentModified
	}
	current := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.documents[uri] == doc
	}
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if !current() {
		return nil, errLSPContentModified
	}
	if shared.writer != c || shared.buffer != doc {
		if err := c.syncShared(ctx, shared, doc); err != nil {
			return nil, err
		}
	}
	changes := shared.changes
	return func() bool {
		shared.mu.Lock()
		defer shared.mu.Unlock()
		return shared.writer == c && shared.changes == changes && current()
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
	case data := <-ch:
		var response struct {
			Result struct {
				Applied bool `json:"applied"`
			} `json:"result"`
			Error *ahp.Error `json:"error"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
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
	s.editor.lspDocumentsMu.Lock()
	shared := s.editor.lspDocuments[path]
	s.editor.lspDocumentsMu.Unlock()
	if shared == nil {
		return nil
	}
	shared.mu.Lock()
	defer shared.mu.Unlock()
	s.editor.lspDocumentsMu.Lock()
	delete(shared.owners, c)
	if _, current := shared.owners[shared.writer]; current {
		s.editor.lspDocumentsMu.Unlock()
		return nil
	}
	owners := make([]*lspConnection, 0, len(shared.owners))
	for owner := range shared.owners {
		owners = append(owners, owner)
	}
	s.editor.lspDocumentsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if len(owners) > 0 {
		for _, owner := range owners {
			owner.mu.Lock()
			doc, open := owner.buffer(path)
			owner.mu.Unlock()
			if open {
				return owner.syncShared(ctx, shared, doc)
			}
		}
		return nil
	}
	shared.writer = nil
	err := c.language.CloseDocument(ctx, path)
	// The entry stays registered during the close so a new owner waits for it.
	s.editor.lspDocumentsMu.Lock()
	if len(shared.owners) == 0 && s.editor.lspDocuments[path] == shared {
		delete(s.editor.lspDocuments, path)
	}
	s.editor.lspDocumentsMu.Unlock()
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
