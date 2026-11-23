package lsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	lspuri "go.lsp.dev/uri"

	"github.com/adrianliechti/wingman-agent/internal/tooling"
)

type Session struct {
	server     Server
	conn       jsonrpc2.Conn
	rpc        protocol.Server
	root       string
	cancelFunc context.CancelFunc

	documentMu   sync.Mutex
	commands     chan struct{} // holds the running command; see acquireCommand
	applyEditMu  sync.Mutex
	mu           sync.Mutex
	documents    map[string]*document
	progress     map[string]bool // work-done progress tokens in flight
	applyEdit    func(CommandWorkspaceEdit) bool
	watchMu      sync.Mutex
	disk         *diskWatches
	watchers     map[string][]fileWatcher // by registration id
	pullDiags    bool
	capabilities protocol.ServerCapabilities
	alive        atomic.Bool
	closeOnce    sync.Once
}

// document tracks what the server has been told about a URI and what it has
// reported back. Servers publish diagnostics for files we never opened, so
// "tracked" and "opened" are distinct.
type document struct {
	opened  bool
	version int
	saved   bool
	editor  bool // kept current, in order, by an editor connection
	content string

	wireDiagnostics []protocol.Diagnostic
	published       bool
}

func (d *document) diskBacked() bool { return d != nil && d.opened && d.saved && !d.editor }

func (s *Session) document(uri string) *document {
	doc := s.documents[uri]
	if doc == nil {
		doc = &document{}
		s.documents[uri] = doc
	}
	return doc
}

func connect(ctx context.Context, workingDir string, server Server) (*Session, error) {
	root, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(server.executable(), server.Args...)
	cmd.Dir = workingDir
	cmd.Env = tooling.Environment(server.executable(), os.Environ())
	for key, value := range server.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stderr = io.Discard

	setSysProcAttr(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start command: %w", err)
	}

	connCtx, cancel := context.WithCancel(context.Background())

	session := &Session{
		server:     server,
		root:       root,
		disk:       newDiskWatches(connCtx, root),
		cancelFunc: cancel,
		documents:  make(map[string]*document),
		progress:   make(map[string]bool),
	}
	session.alive.Store(true)

	_, conn, rpc := protocol.NewClient(
		connCtx,
		&sessionClient{session: session},
		jsonrpc2.NewStream(&cmdStream{cmd: cmd, stdin: stdin, stdout: stdout}),
	)
	session.conn = conn
	session.rpc = rpc

	go func() {
		<-conn.Done()
		session.alive.Store(false)
		cancel()
		session.stopDiskWatches()
	}()

	initCtx, initCancel := context.WithTimeout(ctx, server.startupTimeout())
	defer initCancel()

	if err := session.initialize(initCtx); err != nil {
		_ = conn.Close()
		cancel()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	return session, nil
}

type sessionClient struct {
	protocol.UnimplementedClient
	session *Session
}

func (c *sessionClient) WorkspaceFolders(context.Context) ([]protocol.WorkspaceFolder, error) {
	return c.session.workspaceFolders(), nil
}

func (c *sessionClient) PublishDiagnostics(_ context.Context, params *protocol.PublishDiagnosticsParams) error {
	version, _ := params.Version.Get()
	uri := params.URI.String()
	c.session.mu.Lock()
	if doc := c.session.document(uri); version == 0 || int(version) >= doc.version {
		doc.wireDiagnostics = slices.Clone(params.Diagnostics)
		doc.published = true
	}
	c.session.mu.Unlock()
	return nil
}

func (c *sessionClient) WorkDoneProgressCreate(context.Context, *protocol.WorkDoneProgressCreateParams) error {
	return nil
}

// protocol.Handlers already runs every handler asynchronously, so waiting for
// the user's review here does not hold up the server's other messages.
func (c *sessionClient) ApplyEdit(_ context.Context, params *protocol.ApplyWorkspaceEditParams) (*protocol.ApplyWorkspaceEditResult, error) {
	if params == nil {
		reason := "the language server sent an empty workspace edit request"
		return &protocol.ApplyWorkspaceEditResult{Applied: false, FailureReason: &reason}, nil
	}
	apply, edit, err := c.session.commandEdit(params)
	if err != nil {
		reason := err.Error()
		return &protocol.ApplyWorkspaceEditResult{Applied: false, FailureReason: &reason}, nil
	}
	if !apply(edit) {
		reason := "the workspace edit was not applied"
		return &protocol.ApplyWorkspaceEditResult{Applied: false, FailureReason: &reason}, nil
	}
	return &protocol.ApplyWorkspaceEditResult{Applied: true}, nil
}

func (c *sessionClient) Progress(_ context.Context, params *protocol.ProgressParams) error {
	var value progressUpdate
	if err := protocol.Unmarshal(params.Value, &value); err == nil {
		token, _ := protocol.Marshal(params.Token)
		c.session.applyProgress(string(token), value)
	}
	return nil
}

func (s *Session) IsAlive() bool {
	return s.alive.Load()
}

type progressUpdate struct {
	Kind string `json:"kind"`
}

func (s *Session) applyProgress(token string, update progressUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.progress == nil {
		s.progress = make(map[string]bool)
	}
	switch update.Kind {
	case "begin":
		s.progress[token] = true
	case "end":
		delete(s.progress, token)
	}
}

// Analyzing reports whether the server has announced ongoing background work
// (indexing, loading packages) via work-done progress.
func (s *Session) Analyzing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.progress) > 0
}

type openDocument struct {
	Path    string
	Content string
	Saved   bool
	Editor  bool
}

func (s *Session) openedDocuments() []openDocument {
	s.mu.Lock()
	defer s.mu.Unlock()

	documents := make([]openDocument, 0, len(s.documents))
	for uri, doc := range s.documents {
		if doc.opened {
			path, _ := fileuri.Path(uri)
			documents = append(documents, openDocument{
				Path:    path,
				Content: doc.content,
				Saved:   doc.saved,
				Editor:  doc.editor,
			})
		}
	}
	return documents
}

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.alive.Store(false)
		s.stopDiskWatches()
		ctx, cancel := context.WithTimeout(context.Background(), s.server.shutdownTimeout())
		defer cancel()

		_ = s.rpc.Shutdown(ctx)
		_ = s.rpc.Exit(ctx)
		_ = s.conn.Close()
		s.cancelFunc()
	})
}

func retryRPC[T any](ctx context.Context, call func() (T, error)) (T, error) {
	var result T
	var err error
	for attempt := range maxRetries {
		result, err = call()
		if err == nil || !isTransientError(err) {
			return result, err
		}
		if attempt == maxRetries-1 {
			return result, err
		}
		delay := retryBaseDelay << attempt
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(delay):
		}
	}
	return result, err
}

const maxRetries = 3

var retryBaseDelay = 500 * time.Millisecond

const (
	codeServerCancelled  = -32802
	codeContentModified  = -32801
	codeRequestCancelled = -32800
)

func isTransientError(err error) bool {
	var wireErr *jsonrpc2.Error
	if !errors.As(err, &wireErr) {
		return false
	}

	return wireErr.Code == codeServerCancelled ||
		wireErr.Code == codeContentModified ||
		wireErr.Code == codeRequestCancelled
}

func (s *Session) OpenDocument(ctx context.Context, filePath string) (string, error) {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	return s.openDocumentLocked(ctx, filePath)
}

func (s *Session) openDocumentLocked(ctx context.Context, filePath string) (uri string, err error) {
	uri = fileuri.FromPath(filePath)
	s.mu.Lock()
	doc := s.documents[uri]
	keepBuffer := doc != nil && doc.opened && (doc.editor || !doc.saved)
	opening := doc == nil || !doc.opened
	s.mu.Unlock()
	if keepBuffer {
		// Only the editor can replace its buffer, including after a save.
		return uri, nil
	}
	// Subscribe before the read so a concurrent save cannot fall in between.
	if opening {
		s.watchDiskDocument(filePath, true)
		defer func() {
			if err != nil {
				s.watchDiskDocument(filePath, false)
			}
		}()
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	return s.syncDocumentLocked(ctx, filePath, string(content), true, false)
}

// SyncEditorDocument applies an editor's in-order buffer update and marks the
// document as editor-owned until it is closed.
func (s *Session) SyncEditorDocument(ctx context.Context, filePath, content string, saved bool) (string, error) {
	return s.syncDocument(ctx, filePath, content, saved, true)
}

// SyncRequestDocument makes a request's buffer snapshot available, unless an
// editor already keeps the document current: the snapshot may be older.
func (s *Session) SyncRequestDocument(ctx context.Context, filePath, content string) (string, error) {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	uri := fileuri.FromPath(filePath)
	s.mu.Lock()
	doc := s.documents[uri]
	owned := doc != nil && doc.opened && doc.editor
	s.mu.Unlock()
	if owned {
		return uri, nil
	}
	return s.syncDocumentLocked(ctx, filePath, content, false, false)
}

// CloseDocument ends the lifetime of an editor-owned document. A session can
// also know diagnostics for unopened files, so only documents explicitly
// opened by syncDocument receive didClose.
func (s *Session) CloseDocument(ctx context.Context, filePath string) error {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	return s.closeDocumentLocked(ctx, filePath)
}

func (s *Session) closeDocumentLocked(ctx context.Context, filePath string) error {
	uri := fileuri.FromPath(filePath)
	s.mu.Lock()
	doc := s.documents[uri]
	opened := doc != nil && doc.opened
	wasDisk := doc.diskBacked()
	s.mu.Unlock()
	if !opened {
		return nil
	}
	var err error
	if s.documentSyncPolicy().openClose && s.IsAlive() {
		err = s.rpc.DidClose(ctx, &protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: lspuri.MustParse(uri)},
		})
	}

	s.mu.Lock()
	delete(s.documents, uri)
	s.mu.Unlock()
	if wasDisk {
		s.watchDiskDocument(filePath, false)
	}
	if err != nil {
		return fmt.Errorf("didClose: %w", err)
	}
	return nil
}

// forgetDocument drops a document without telling the stopped server.
func (s *Session) forgetDocument(filePath string) {
	s.mu.Lock()
	delete(s.documents, fileuri.FromPath(filePath))
	s.mu.Unlock()
}

func (s *Session) DocumentContent(filePath string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.documents[fileuri.FromPath(filePath)]
	if doc == nil || !doc.opened {
		return "", false
	}
	return doc.content, true
}

func (s *Session) syncDocument(ctx context.Context, filePath, content string, saved, editor bool) (string, error) {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	return s.syncDocumentLocked(ctx, filePath, content, saved, editor)
}

func (s *Session) syncDocumentLocked(ctx context.Context, filePath, content string, saved, editor bool) (string, error) {
	uri := fileuri.FromPath(filePath)

	s.mu.Lock()
	doc := s.document(uri)
	opened, unchanged, wasSaved := doc.opened, doc.content == content, doc.saved
	wasDisk := doc.diskBacked()
	version := doc.version + 1
	if opened && !unchanged {
		doc.version = version
		doc.wireDiagnostics = nil
		doc.published = false
	}
	s.mu.Unlock()

	policy := s.documentSyncPolicy()
	notifySaved := func() error {
		if !policy.save {
			return nil
		}
		var text *string
		if policy.includeText {
			text = &content
		}
		if err := s.rpc.DidSave(ctx, &protocol.DidSaveTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: lspuri.MustParse(uri)},
			Text:         text,
		}); err != nil {
			return fmt.Errorf("didSave: %w", err)
		}
		return nil
	}

	switch {
	case opened && unchanged:
		if saved && !wasSaved {
			if err := notifySaved(); err != nil {
				return "", err
			}
		}

	case opened:
		if changes := documentChanges(policy.change, doc.content, content); len(changes) > 0 {
			if err := s.rpc.DidChange(ctx, &protocol.DidChangeTextDocumentParams{
				TextDocument: protocol.VersionedTextDocumentIdentifier{
					TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: lspuri.MustParse(uri)},
					Version:                int32(version),
				},
				ContentChanges: changes,
			}); err != nil {
				return "", fmt.Errorf("didChange: %w", err)
			}
		}
		if saved {
			if err := notifySaved(); err != nil {
				return "", err
			}
		}

	default:
		if policy.openClose {
			if err := s.rpc.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
				TextDocument: protocol.TextDocumentItem{
					URI:        lspuri.MustParse(uri),
					LanguageID: protocol.LanguageKind(s.server.LanguageIDForPath(filePath)),
					Version:    1,
					Text:       content,
				},
			}); err != nil {
				return "", fmt.Errorf("didOpen: %w", err)
			}
		}
	}

	s.mu.Lock()
	doc = s.document(uri)
	doc.opened = true
	doc.saved = saved || (opened && unchanged && wasSaved && !editor)
	doc.editor = doc.editor || editor
	doc.content = content
	isDisk := doc.diskBacked()
	if !opened {
		doc.version = 1
	}
	s.mu.Unlock()
	if wasDisk != isDisk {
		s.watchDiskDocument(filePath, isDisk)
	}

	return uri, nil
}

type documentSyncPolicy struct {
	openClose   bool
	change      protocol.TextDocumentSyncKind
	save        bool
	includeText bool
}

func (s *Session) documentSyncPolicy() documentSyncPolicy {
	switch sync := s.capabilities.TextDocumentSync.(type) {
	case protocol.TextDocumentSyncKind:
		return documentSyncPolicy{
			openClose: sync != protocol.TextDocumentSyncKindNone,
			change:    sync,
		}
	case *protocol.TextDocumentSyncOptions:
		policy := documentSyncPolicy{}
		if sync.OpenClose != nil {
			policy.openClose = *sync.OpenClose
		}
		if sync.Change != nil {
			policy.change = *sync.Change
		}
		switch save := sync.Save.(type) {
		case protocol.Boolean:
			policy.save = bool(save)
		case *protocol.SaveOptions:
			policy.save = true
			policy.includeText = save.IncludeText != nil && *save.IncludeText
		}
		return policy
	default:
		return documentSyncPolicy{}
	}
}

func documentChanges(kind protocol.TextDocumentSyncKind, previous, content string) []protocol.TextDocumentContentChangeEvent {
	switch kind {
	case protocol.TextDocumentSyncKindFull:
		return []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangeWholeDocument{Text: content},
		}
	case protocol.TextDocumentSyncKindIncremental:
		return []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangePartial{
				Range: protocol.Range{
					Start: protocol.Position{},
					End:   documentEndPosition(previous),
				},
				Text: content,
			},
		}
	default:
		return nil
	}
}

func documentEndPosition(content string) protocol.Position {
	line := strings.Count(content, "\n")
	lastLine := content
	if newline := strings.LastIndexByte(content, '\n'); newline >= 0 {
		lastLine = content[newline+1:]
	}
	return protocol.Position{
		Line:      uint32(line),
		Character: uint32(len(utf16.Encode([]rune(lastLine)))),
	}
}

func (s *Session) publishedProtocolDiagnostics(uri string) ([]protocol.Diagnostic, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc := s.documents[uri]
	if doc == nil {
		return nil, false
	}
	return slices.Clone(doc.wireDiagnostics), doc.published
}

func (s *Session) pullProtocolDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, bool) {
	callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
	defer callCancel()
	report, err := retryRPC(callCtx, func() (protocol.DocumentDiagnosticReport, error) {
		return s.rpc.Diagnostic(callCtx, &protocol.DocumentDiagnosticParams{TextDocument: wireDocument(uri)})
	})
	if err != nil {
		return nil, false
	}
	switch report := report.(type) {
	case *protocol.RelatedFullDocumentDiagnosticReport:
		return report.Items, true
	case *protocol.RelatedUnchangedDocumentDiagnosticReport:
		return s.publishedProtocolDiagnostics(uri)
	}
	return nil, false
}

// DiagnosticsState reports the diagnostics for uri and whether the server has
// actually produced a result: known=false means the server neither published
// nor answered a pull request, so "no diagnostics" cannot be concluded.
func (s *Session) DiagnosticsState(ctx context.Context, uri string) ([]Diagnostic, bool) {
	if published, ok := s.publishedProtocolDiagnostics(uri); ok {
		return published, true
	}
	if s.pullDiags {
		return s.pullProtocolDiagnostics(ctx, uri)
	}
	return nil, false
}

// DiagnosticsForEdit returns a report only if the server kept the exact text
// written by the edit throughout the request. An editor can own the same URI.
func (s *Session) DiagnosticsForEdit(ctx context.Context, filePath, editedText string, timeout time.Duration) []Diagnostic {
	uri, err := s.OpenDocument(ctx, filePath)
	if err != nil {
		return nil
	}
	doc, version := s.revisionForText(uri, editedText)
	if doc == nil {
		return nil
	}
	diagnostics, _ := s.WaitForDiagnostics(ctx, uri, timeout)
	if current, currentVersion := s.revisionForText(uri, editedText); current != doc || currentVersion != version {
		return nil
	}
	return diagnostics
}

// Take a snapshot between completed document updates. A close and reopen
// replaces the document and starts its version counter at one again.
func (s *Session) revisionForText(uri, text string) (*document, int) {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if doc := s.documents[uri]; doc != nil && doc.opened && doc.content == text {
		return doc, doc.version
	}
	return nil, 0
}

func (s *Session) WaitForDiagnostics(ctx context.Context, uri string, timeout time.Duration) ([]Diagnostic, bool) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	for {
		if diags, known := s.DiagnosticsState(waitCtx, uri); known {
			return diags, true
		}

		select {
		case <-waitCtx.Done():
			return nil, false
		case <-ticker.C:
		}
	}
}

func (s *Session) DefinitionLocations(ctx context.Context, uri string, line, column int) ([]Location, error) {
	result, err := retryRPC(ctx, func() (protocol.DefinitionResult, error) {
		return s.rpc.Definition(ctx, &protocol.DefinitionParams{TextDocumentPositionParams: documentPosition(uri, line, column)})
	})
	return locationsFromDefinition(result), err
}

func (s *Session) TypeDefinitionLocations(ctx context.Context, uri string, line, column int) ([]Location, error) {
	result, err := retryRPC(ctx, func() (protocol.DefinitionResult, error) {
		return s.rpc.TypeDefinition(ctx, &protocol.TypeDefinitionParams{TextDocumentPositionParams: documentPosition(uri, line, column)})
	})
	return locationsFromDefinition(result), err
}

func (s *Session) ImplementationLocations(ctx context.Context, uri string, line, column int) ([]Location, error) {
	result, err := retryRPC(ctx, func() (protocol.DefinitionResult, error) {
		return s.rpc.Implementation(ctx, &protocol.ImplementationParams{TextDocumentPositionParams: documentPosition(uri, line, column)})
	})
	return locationsFromDefinition(result), err
}

func (s *Session) ReferenceLocations(ctx context.Context, uri string, line, column int, includeDeclaration bool) ([]Location, error) {
	result, err := retryRPC(ctx, func() ([]protocol.Location, error) {
		return s.rpc.References(ctx, &protocol.ReferenceParams{
			TextDocumentPositionParams: documentPosition(uri, line, column),
			Context:                    protocol.ReferenceContext{IncludeDeclaration: includeDeclaration},
		})
	})
	return []Location(result), err
}

func (s *Session) Hover(ctx context.Context, uri string, line, column int) (*Hover, error) {
	return retryRPC(ctx, func() (*protocol.Hover, error) {
		return s.rpc.Hover(ctx, &protocol.HoverParams{TextDocumentPositionParams: documentPosition(uri, line, column)})
	})
}

func (s *Session) CompletionItems(ctx context.Context, uri string, line, column int, completionContext *CompletionContext) (CompletionList, error) {
	params := &protocol.CompletionParams{TextDocumentPositionParams: documentPosition(uri, line, column)}
	if completionContext != nil {
		params.Context = *completionContext
	}
	result, err := retryRPC(ctx, func() (protocol.CompletionResult, error) {
		return s.rpc.Completion(ctx, params)
	})
	if err != nil {
		return CompletionList{}, err
	}
	switch result := result.(type) {
	case protocol.CompletionItemSlice:
		return protocol.CompletionList{Items: []protocol.CompletionItem(result)}, nil
	case *protocol.CompletionList:
		return *result, nil
	default:
		return protocol.CompletionList{}, nil
	}
}

func (s *Session) ResolveCompletionItem(ctx context.Context, item CompletionItem) (CompletionItem, error) {
	result, err := retryRPC(ctx, func() (*protocol.CompletionItem, error) {
		return s.rpc.CompletionResolve(ctx, &item)
	})
	if err != nil {
		return CompletionItem{}, err
	}
	if result == nil {
		return item, nil
	}
	return *result, nil
}

func (s *Session) SignatureHelp(ctx context.Context, uri string, line, column int, signatureContext *SignatureHelpContext) (*SignatureHelp, error) {
	params := &protocol.SignatureHelpParams{TextDocumentPositionParams: documentPosition(uri, line, column)}
	if signatureContext != nil {
		params.Context = *signatureContext
	}
	help, err := retryRPC(ctx, func() (*protocol.SignatureHelp, error) {
		return s.rpc.SignatureHelp(ctx, params)
	})
	if err != nil {
		return nil, err
	}
	return help, nil
}

func (s *Session) DocumentSymbols(ctx context.Context, uri string) (DocumentSymbolResult, error) {
	return retryRPC(ctx, func() (protocol.DocumentSymbolResult, error) {
		return s.rpc.DocumentSymbol(ctx, &protocol.DocumentSymbolParams{TextDocument: wireDocument(uri)})
	})
}

func (s *Session) WorkspaceSymbols(ctx context.Context, query string) ([]WorkspaceSymbol, error) {
	ctx, cancel := context.WithTimeout(ctx, s.RequestTimeout())
	defer cancel()
	result, err := retryRPC(ctx, func() (protocol.WorkspaceSymbolResult, error) {
		return s.rpc.Symbols(ctx, &protocol.WorkspaceSymbolParams{Query: query})
	})
	if err != nil {
		return nil, err
	}
	switch symbols := result.(type) {
	case protocol.WorkspaceSymbolSlice:
		return symbols, nil
	case protocol.SymbolInformationSlice:
		converted := make([]WorkspaceSymbol, len(symbols))
		for i, symbol := range symbols {
			converted[i] = WorkspaceSymbol{BaseSymbolInformation: symbol.BaseSymbolInformation, Location: &symbols[i].Location}
		}
		return converted, nil
	default:
		return nil, nil
	}
}

func (s *Session) DocumentHighlights(ctx context.Context, uri string, line, column int) ([]DocumentHighlight, error) {
	result, err := retryRPC(ctx, func() ([]protocol.DocumentHighlight, error) {
		return s.rpc.DocumentHighlight(ctx, &protocol.DocumentHighlightParams{TextDocumentPositionParams: documentPosition(uri, line, column)})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Session) FoldingRanges(ctx context.Context, uri string) ([]FoldingRange, error) {
	result, err := retryRPC(ctx, func() ([]protocol.FoldingRange, error) {
		return s.rpc.FoldingRanges(ctx, &protocol.FoldingRangeParams{TextDocument: wireDocument(uri)})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Session) SemanticTokens(ctx context.Context, uri string) (*SemanticTokens, error) {
	return retryRPC(ctx, func() (*protocol.SemanticTokens, error) {
		return s.rpc.SemanticTokensFull(ctx, &protocol.SemanticTokensParams{TextDocument: wireDocument(uri)})
	})
}

func (s *Session) PrepareCallHierarchy(ctx context.Context, uri string, line, column int) ([]CallHierarchyItem, error) {
	return retryRPC(ctx, func() ([]protocol.CallHierarchyItem, error) {
		return s.rpc.PrepareCallHierarchy(ctx, &protocol.CallHierarchyPrepareParams{TextDocumentPositionParams: documentPosition(uri, line, column)})
	})
}

func (s *Session) workspaceFolders() []protocol.WorkspaceFolder {
	return []protocol.WorkspaceFolder{{URI: lspuri.MustParse(fileuri.FromPath(s.root)), Name: filepath.Base(s.root)}}
}

func (s *Session) initialize(ctx context.Context) error {
	processID := int32(os.Getpid())
	rootURI := lspuri.MustParse(fileuri.FromPath(s.root))
	params := &protocol.InitializeParams{
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable(s.workspaceFolders()),
		},
		ProcessID: &processID,
		//lint:ignore SA1019 servers that predate workspace folders read only rootUri
		RootURI:               &rootURI,
		InitializationOptions: protocol.LSPAny(s.server.InitializationOptions),
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				ApplyEdit:        new(true),
				WorkspaceFolders: new(true),
				DidChangeWatchedFiles: &protocol.DidChangeWatchedFilesClientCapabilities{
					DynamicRegistration:    new(true),
					RelativePatternSupport: new(true),
				},
				TextDocumentContent: &protocol.TextDocumentContentClientCapabilities{},
				WorkspaceEdit: &protocol.WorkspaceEditClientCapabilities{
					DocumentChanges:         new(true),
					FailureHandling:         protocol.FailureHandlingKindTextOnlyTransactional,
					NormalizesLineEndings:   new(false),
					ChangeAnnotationSupport: &protocol.ChangeAnnotationsSupportOptions{},
				},
			},
			TextDocument: &protocol.TextDocumentClientCapabilities{
				Synchronization: &protocol.TextDocumentSyncClientCapabilities{DidSave: new(true)},
				Completion: &protocol.CompletionClientCapabilities{
					ContextSupport: new(true),
					CompletionItem: &protocol.ClientCompletionItemOptions{
						SnippetSupport:          new(true),
						CommitCharactersSupport: new(true),
						PreselectSupport:        new(true),
						InsertReplaceSupport:    new(true),
						DocumentationFormat:     []protocol.MarkupKind{protocol.MarkupKindMarkdown, protocol.MarkupKindPlainText},
						ResolveSupport: protocol.ClientCompletionItemResolveOptions{
							Properties: []string{"documentation", "detail", "additionalTextEdits", "command"},
						},
					},
				},
				SignatureHelp: &protocol.SignatureHelpClientCapabilities{
					ContextSupport: new(true),
					SignatureInformation: &protocol.ClientSignatureInformationOptions{
						DocumentationFormat:    []protocol.MarkupKind{protocol.MarkupKindMarkdown, protocol.MarkupKindPlainText},
						ParameterInformation:   &protocol.ClientSignatureParameterInformationOptions{LabelOffsetSupport: new(true)},
						ActiveParameterSupport: new(true),
					},
				},
				Hover:             &protocol.HoverClientCapabilities{ContentFormat: []protocol.MarkupKind{protocol.MarkupKindMarkdown, protocol.MarkupKindPlainText}},
				Definition:        &protocol.DefinitionClientCapabilities{LinkSupport: new(true)},
				TypeDefinition:    &protocol.TypeDefinitionClientCapabilities{},
				References:        &protocol.ReferenceClientCapabilities{},
				Implementation:    &protocol.ImplementationClientCapabilities{},
				DocumentSymbol:    &protocol.DocumentSymbolClientCapabilities{HierarchicalDocumentSymbolSupport: new(true)},
				DocumentHighlight: &protocol.DocumentHighlightClientCapabilities{},
				FoldingRange:      &protocol.FoldingRangeClientCapabilities{},
				Rename:            &protocol.RenameClientCapabilities{PrepareSupport: new(true), HonorsChangeAnnotations: new(true)},
				CodeAction: &protocol.CodeActionClientCapabilities{
					IsPreferredSupport:      new(true),
					DisabledSupport:         new(true),
					DataSupport:             new(true),
					HonorsChangeAnnotations: new(true),
					CodeActionLiteralSupport: protocol.ClientCodeActionLiteralOptions{
						CodeActionKind: protocol.ClientCodeActionKindOptions{ValueSet: []protocol.CodeActionKind{
							protocol.CodeActionKindQuickFix,
							protocol.CodeActionKindRefactor,
							protocol.CodeActionKindRefactorExtract,
							protocol.CodeActionKindRefactorInline,
							protocol.CodeActionKindRefactorMove,
							protocol.CodeActionKindRefactorRewrite,
							protocol.CodeActionKindSource,
							protocol.CodeActionKindSourceOrganizeImports,
							protocol.CodeActionKindSourceFixAll,
						}},
					},
					ResolveSupport: protocol.ClientCodeActionResolveOptions{Properties: []string{"edit", "command"}},
				},
				Formatting:       &protocol.DocumentFormattingClientCapabilities{},
				RangeFormatting:  &protocol.DocumentRangeFormattingClientCapabilities{},
				OnTypeFormatting: &protocol.DocumentOnTypeFormattingClientCapabilities{},
				SemanticTokens: protocol.SemanticTokensClientCapabilities{
					Requests: protocol.ClientSemanticTokensRequestOptions{
						Full:  protocol.Boolean(true),
						Range: protocol.Boolean(true),
					},
					TokenTypes: []string{
						"namespace", "type", "class", "enum", "interface", "struct",
						"typeParameter", "parameter", "variable", "property", "enumMember",
						"event", "function", "method", "macro", "label", "comment", "string",
						"keyword", "number", "regexp", "operator", "decorator",
					},
					TokenModifiers: []string{
						"declaration", "definition", "readonly", "static", "deprecated",
						"abstract", "async", "modification", "documentation", "defaultLibrary",
					},
					Formats: []protocol.TokenFormat{protocol.TokenFormatRelative},
				},
				InlayHint:     &protocol.InlayHintClientCapabilities{},
				Diagnostic:    &protocol.DiagnosticClientCapabilities{},
				CallHierarchy: &protocol.CallHierarchyClientCapabilities{},
			},
			Window: &protocol.WindowClientCapabilities{WorkDoneProgress: new(true)},
		},
	}

	result, err := s.rpc.Initialize(ctx, params)
	if err != nil {
		return err
	}
	s.pullDiags = result.Capabilities.DiagnosticProvider != nil
	s.capabilities = result.Capabilities

	if err := s.rpc.Initialized(ctx, &protocol.InitializedParams{}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	if len(s.server.Settings) > 0 {
		if err := s.rpc.DidChangeConfiguration(ctx, &protocol.DidChangeConfigurationParams{Settings: protocol.LSPAny(s.server.Settings)}); err != nil {
			return fmt.Errorf("configuration notification: %w", err)
		}
	}

	return nil
}

// RequestTimeout is how long the server is given to answer a request.
func (s *Session) RequestTimeout() time.Duration {
	return s.server.requestTimeout()
}

func (s *Session) Capabilities() protocol.ServerCapabilities {
	return s.capabilities
}

func (s *Session) IncomingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyIncomingCall, error) {
	return retryRPC(ctx, func() ([]protocol.CallHierarchyIncomingCall, error) {
		return s.rpc.IncomingCalls(ctx, &protocol.CallHierarchyIncomingCallsParams{Item: item})
	})
}

func (s *Session) OutgoingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyOutgoingCall, error) {
	return retryRPC(ctx, func() ([]protocol.CallHierarchyOutgoingCall, error) {
		return s.rpc.OutgoingCalls(ctx, &protocol.CallHierarchyOutgoingCallsParams{Item: item})
	})
}

type cmdStream struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (c *cmdStream) Read(p []byte) (int, error) {
	return c.stdout.Read(p)
}

func (c *cmdStream) Write(p []byte) (int, error) {
	return c.stdin.Write(p)
}

func (c *cmdStream) Close() error {
	c.stdin.Close()
	c.stdout.Close()

	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}

	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout waiting for LSP process to exit")
	}
}
