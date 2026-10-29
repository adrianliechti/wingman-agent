package lsp

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
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
	cmd        *exec.Cmd
	rootURI    string
	cancelFunc context.CancelFunc

	documentMu   sync.Mutex
	commandMu    sync.Mutex
	applyEditMu  sync.Mutex
	mu           sync.Mutex
	documents    map[string]*document
	progress     map[string]WorkProgress
	applyEdit    func(CommandWorkspaceEdit) bool
	created      time.Time
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
	sum     uint64
	version int
	saved   bool
	editor  bool // kept current, in order, by an editor connection
	content string
	disk    fileStamp // last disk state read into a disk-backed document

	wireDiagnostics []protocol.Diagnostic
	published       bool
}

func (s *Session) document(uri string) *document {
	doc := s.documents[uri]
	if doc == nil {
		doc = &document{}
		s.documents[uri] = doc
	}
	return doc
}

const startupTimeout = 30 * time.Second

func connect(ctx context.Context, workingDir string, server Server) (*Session, error) {
	cmd := exec.Command(server.Command, server.Args...)
	cmd.Dir = workingDir
	cmd.Env = tooling.Environment(server.Command, os.Environ())
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
		cmd:        cmd,
		rootURI:    fileuri.FromPath(workingDir),
		cancelFunc: cancel,
		documents:  make(map[string]*document),
		progress:   make(map[string]WorkProgress),
		created:    time.Now(),
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
	}()

	initCtx, initCancel := context.WithTimeout(ctx, startupTimeout)
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
	// The server's further messages wait while the user reviews the edit;
	// jsonrpc2.Async would release the reader but breaks the stdio stream.
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

// WorkProgress is one server-reported background operation. Title is stable
// for the lifetime of an operation; Message and Percentage may be updated by
// subsequent progress reports.
type WorkProgress struct {
	Title      string  `json:"title,omitempty"`
	Message    string  `json:"message,omitempty"`
	Percentage *uint32 `json:"percentage,omitempty"`
}

type progressUpdate struct {
	Kind       string  `json:"kind"`
	Title      string  `json:"title,omitempty"`
	Message    *string `json:"message,omitempty"`
	Percentage *uint32 `json:"percentage,omitempty"`
}

func (s *Session) applyProgress(token string, update progressUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.progress == nil {
		s.progress = make(map[string]WorkProgress)
	}
	switch update.Kind {
	case "begin":
		progress := WorkProgress{Title: update.Title}
		applyProgressUpdate(&progress, update)
		s.progress[token] = progress
	case "report":
		progress := s.progress[token]
		applyProgressUpdate(&progress, update)
		s.progress[token] = progress
	case "end":
		delete(s.progress, token)
	}
}

func applyProgressUpdate(progress *WorkProgress, update progressUpdate) {
	if update.Title != "" {
		progress.Title = update.Title
	}
	if update.Message != nil {
		progress.Message = *update.Message
	}
	if update.Percentage != nil {
		percentage := *update.Percentage
		progress.Percentage = &percentage
	}
}

// Analyzing reports whether the server has announced ongoing background work
// (indexing, loading packages) via work-done progress.
func (s *Session) Analyzing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.progress) > 0
}

func (s *Session) Progress() []WorkProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]WorkProgress, 0, len(s.progress))
	for _, progress := range s.progress {
		if progress.Percentage != nil {
			percentage := *progress.Percentage
			progress.Percentage = &percentage
		}
		result = append(result, progress)
	}
	slices.SortFunc(result, func(a, b WorkProgress) int {
		if order := strings.Compare(a.Title, b.Title); order != 0 {
			return order
		}
		return strings.Compare(a.Message, b.Message)
	})
	return result
}

func (s *Session) Age() time.Duration {
	return time.Since(s.created)
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

type fileStamp struct {
	size    int64
	modTime time.Time
}

func statFile(filePath string) (fileStamp, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{size: info.Size(), modTime: info.ModTime()}, nil
}

func (s *Session) OpenDocument(ctx context.Context, filePath string) (string, error) {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	return s.openDocumentLocked(ctx, filePath)
}

func (s *Session) openDocumentLocked(ctx context.Context, filePath string) (string, error) {
	uri := fileuri.FromPath(filePath)
	s.mu.Lock()
	doc := s.documents[uri]
	dirty := doc != nil && doc.opened && !doc.saved
	s.mu.Unlock()
	if dirty {
		// A disk-backed request must never replace an unsaved buffer.
		return uri, nil
	}
	stamp, _ := statFile(filePath)
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	uri, err = s.syncDocumentLocked(ctx, filePath, content, true, false)
	if err == nil {
		s.mu.Lock()
		s.document(uri).disk = stamp
		s.mu.Unlock()
	}
	return uri, err
}

// RefreshDiskDocuments keeps documents opened from disk current: while open,
// a server must not read them itself. Editor-owned documents are left to the
// editor, and deleted files are closed.
func (s *Session) RefreshDiskDocuments(ctx context.Context) {
	s.mu.Lock()
	stale := map[string]fileStamp{}
	for uri, doc := range s.documents {
		if path, ok := fileuri.Path(uri); ok && doc.opened && doc.saved && !doc.editor {
			stale[path] = doc.disk
		}
	}
	s.mu.Unlock()
	for path, previous := range stale {
		stamp, err := statFile(path)
		if err == nil && stamp == previous {
			continue
		}
		s.documentMu.Lock()
		s.mu.Lock()
		doc := s.documents[fileuri.FromPath(path)]
		current := doc != nil && doc.opened && doc.saved && !doc.editor
		s.mu.Unlock()
		if current {
			if errors.Is(err, fs.ErrNotExist) {
				_ = s.closeDocumentLocked(ctx, path)
			} else if err == nil {
				_, _ = s.openDocumentLocked(ctx, path)
			}
		}
		s.documentMu.Unlock()
	}
}

// SyncDocument updates the language server with an editor buffer without
// claiming that the buffer has been saved to disk.
func (s *Session) SyncDocument(ctx context.Context, filePath, content string) (string, error) {
	return s.syncDocument(ctx, filePath, []byte(content), false, false)
}

// SaveDocument synchronizes an editor buffer and explicitly tells the server
// that the same content has been persisted to disk.
func (s *Session) SaveDocument(ctx context.Context, filePath, content string) (string, error) {
	return s.syncDocument(ctx, filePath, []byte(content), true, false)
}

// SyncEditorDocument applies an editor's in-order buffer update and marks the
// document as editor-owned until it is closed.
func (s *Session) SyncEditorDocument(ctx context.Context, filePath, content string, saved bool) (string, error) {
	return s.syncDocument(ctx, filePath, []byte(content), saved, true)
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
	return s.syncDocumentLocked(ctx, filePath, []byte(content), false, false)
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
	s.mu.Unlock()
	if !opened {
		return nil
	}
	if s.documentSyncPolicy().openClose {
		if err := s.rpc.DidClose(ctx, &protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: lspuri.MustParse(uri)},
		}); err != nil {
			return fmt.Errorf("didClose: %w", err)
		}
	}

	s.mu.Lock()
	delete(s.documents, uri)
	s.mu.Unlock()
	return nil
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

// DocumentSnapshot identifies the text/version used for an LSP edit preview.
func (s *Session) DocumentSnapshot(filePath string) (string, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.documents[fileuri.FromPath(filePath)]
	if doc == nil || !doc.opened {
		return "", 0, false
	}
	return doc.content, doc.version, true
}

func (s *Session) syncDocument(ctx context.Context, filePath string, content []byte, saved, editor bool) (string, error) {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	return s.syncDocumentLocked(ctx, filePath, content, saved, editor)
}

func (s *Session) syncDocumentLocked(ctx context.Context, filePath string, content []byte, saved, editor bool) (string, error) {
	uri := fileuri.FromPath(filePath)

	h := fnv.New64a()
	h.Write(content)
	sum := h.Sum64()

	s.mu.Lock()
	doc := s.document(uri)
	opened, unchanged, wasSaved := doc.opened, doc.sum == sum, doc.saved
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
			value := string(content)
			text = &value
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
		if !saved || wasSaved {
			return uri, nil
		}
		if err := notifySaved(); err != nil {
			return "", err
		}

	case opened:
		if changes := documentChanges(policy.change, doc.content, string(content)); len(changes) > 0 {
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
					Text:       string(content),
				},
			}); err != nil {
				return "", fmt.Errorf("didOpen: %w", err)
			}
		}
	}

	s.mu.Lock()
	doc = s.document(uri)
	doc.opened = true
	doc.sum = sum
	doc.saved = saved
	doc.editor = doc.editor || editor
	doc.content = string(content)
	if !opened {
		doc.version = 1
	}
	s.mu.Unlock()

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

func (s *Session) PushSeen(uri string) bool {
	_, published := s.publishedProtocolDiagnostics(uri)
	return published
}

func (s *Session) SupportsPullDiagnostics() bool {
	return s.pullDiags
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

func (s *Session) protocolDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, bool) {
	if published, ok := s.publishedProtocolDiagnostics(uri); ok {
		return published, true
	}
	if s.pullDiags {
		return s.pullProtocolDiagnostics(ctx, uri)
	}
	return nil, false
}

// DiagnosticsState reports the diagnostics for uri and whether the server has
// actually produced a result: known=false means the server neither published
// nor answered a pull request, so "no diagnostics" cannot be concluded.
func (s *Session) DiagnosticsState(ctx context.Context, uri string) ([]Diagnostic, bool) {
	return s.protocolDiagnostics(ctx, uri)
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

const (
	methodDefinition     = "textDocument/definition"
	methodTypeDefinition = "textDocument/typeDefinition"
	methodImplementation = "textDocument/implementation"
	methodReferences     = "textDocument/references"
)

// locations runs any of the position-based navigation requests, all of which
// answer with Location | Location[] | LocationLink[].
func (s *Session) locations(ctx context.Context, method, uri string, line, column int) ([]Location, error) {
	documentPosition := protocol.TextDocumentPositionParams{
		TextDocument: wireDocument(uri),
		Position:     wirePosition(line, column),
	}
	switch method {
	case methodDefinition:
		result, err := retryRPC(ctx, func() (protocol.DefinitionResult, error) {
			return s.rpc.Definition(ctx, &protocol.DefinitionParams{TextDocumentPositionParams: documentPosition})
		})
		return locationsFromDefinition(result), err
	case methodTypeDefinition:
		result, err := retryRPC(ctx, func() (protocol.DefinitionResult, error) {
			return s.rpc.TypeDefinition(ctx, &protocol.TypeDefinitionParams{TextDocumentPositionParams: documentPosition})
		})
		return locationsFromDefinition(result), err
	case methodImplementation:
		result, err := retryRPC(ctx, func() (protocol.DefinitionResult, error) {
			return s.rpc.Implementation(ctx, &protocol.ImplementationParams{TextDocumentPositionParams: documentPosition})
		})
		return locationsFromDefinition(result), err
	case methodReferences:
		return s.ReferenceLocations(ctx, uri, line, column)
	default:
		return nil, fmt.Errorf("unsupported location method %q", method)
	}
}

func (s *Session) DefinitionLocations(ctx context.Context, uri string, line, column int) ([]Location, error) {
	return s.locations(ctx, methodDefinition, uri, line, column)
}

func (s *Session) TypeDefinitionLocations(ctx context.Context, uri string, line, column int) ([]Location, error) {
	return s.locations(ctx, methodTypeDefinition, uri, line, column)
}

func (s *Session) ImplementationLocations(ctx context.Context, uri string, line, column int) ([]Location, error) {
	return s.locations(ctx, methodImplementation, uri, line, column)
}

func (s *Session) ReferenceLocations(ctx context.Context, uri string, line, column int, includeDeclaration ...bool) ([]Location, error) {
	include := true
	if len(includeDeclaration) > 0 {
		include = includeDeclaration[0]
	}
	result, err := retryRPC(ctx, func() ([]protocol.Location, error) {
		return s.rpc.References(ctx, &protocol.ReferenceParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{TextDocument: wireDocument(uri), Position: wirePosition(line, column)},
			Context:                    protocol.ReferenceContext{IncludeDeclaration: include},
		})
	})
	return []Location(result), err
}

func (s *Session) Hover(ctx context.Context, uri string, line, column int) (*Hover, error) {
	return retryRPC(ctx, func() (*protocol.Hover, error) {
		return s.rpc.Hover(ctx, &protocol.HoverParams{TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: wireDocument(uri),
			Position:     wirePosition(line, column),
		}})
	})
}

func (s *Session) CompletionItems(ctx context.Context, uri string, line, column int, completionContext *CompletionContext) (CompletionList, error) {
	params := &protocol.CompletionParams{TextDocumentPositionParams: protocol.TextDocumentPositionParams{
		TextDocument: wireDocument(uri),
		Position:     wirePosition(line, column),
	}}
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
	params := &protocol.SignatureHelpParams{TextDocumentPositionParams: protocol.TextDocumentPositionParams{
		TextDocument: wireDocument(uri),
		Position:     wirePosition(line, column),
	}}
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

func (s *Session) WorkspaceSymbols(ctx context.Context, query string) (WorkspaceSymbolResult, error) {
	return retryRPC(ctx, func() (protocol.WorkspaceSymbolResult, error) {
		return s.rpc.Symbols(ctx, &protocol.WorkspaceSymbolParams{Query: query})
	})
}

func (s *Session) DocumentHighlights(ctx context.Context, uri string, line, column int) ([]DocumentHighlight, error) {
	result, err := retryRPC(ctx, func() ([]protocol.DocumentHighlight, error) {
		return s.rpc.DocumentHighlight(ctx, &protocol.DocumentHighlightParams{TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: wireDocument(uri),
			Position:     wirePosition(line, column),
		}})
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
		return s.rpc.PrepareCallHierarchy(ctx, &protocol.CallHierarchyPrepareParams{TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: wireDocument(uri),
			Position:     wirePosition(line, column),
		}})
	})
}

func (s *Session) workspaceFolders() []protocol.WorkspaceFolder {
	path, _ := fileuri.Path(s.rootURI)
	return []protocol.WorkspaceFolder{{URI: lspuri.MustParse(s.rootURI), Name: filepath.Base(path)}}
}

func (s *Session) initialize(ctx context.Context) error {
	processID := int32(os.Getpid())
	rootURI := lspuri.MustParse(s.rootURI)
	params := &protocol.InitializeParams{
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable(s.workspaceFolders()),
		},
		ProcessID:             &processID,
		RootURI:               &rootURI,
		InitializationOptions: protocol.LSPAny(s.server.InitializationOptions),
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				ApplyEdit:        new(true),
				WorkspaceFolders: new(true),
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

	return nil
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
