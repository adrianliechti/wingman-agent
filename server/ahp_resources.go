package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const ahpResourceLimit = 16 << 20

func (h *ahpHost) resourcePath(uri string) (string, error) {
	if h.server.workspace == nil {
		return "", ahpInvalid("workspace unavailable")
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Host != "" && u.Host != "localhost" || u.RawQuery != "" || u.Fragment != "" {
		return "", ahpInvalid("local file URI required")
	}
	path := filepath.FromSlash(u.Path)
	if !filepath.IsAbs(path) {
		return "", ahpInvalid("absolute file URI required")
	}
	rel, err := filepath.Rel(h.server.workspace.RootPath, path)
	if err != nil || rel != "." && !filepath.IsLocal(rel) {
		// macOS commonly exposes the same root through /var and /private/var.
		if canonicalRoot, resolveErr := filepath.EvalSymlinks(h.server.workspace.RootPath); resolveErr == nil {
			rel, err = filepath.Rel(canonicalRoot, path)
		}
	}
	if err != nil || rel != "." && !filepath.IsLocal(rel) {
		return "", ahpInvalid("resource is outside this Wingman workspace")
	}
	return rel, nil
}
func (h *ahpHost) validateDirectory(uri string) error {
	rel, err := h.resourcePath(uri)
	if err != nil {
		return err
	}
	if rel != "." {
		return ahpInvalid("start wingman server in the directory you want to use")
	}
	return nil
}
func ahpResourceError(err error) error {
	if err == nil {
		return nil
	}
	code := 0
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code = int(t.ErrorCodeNotFound)
	case errors.Is(err, fs.ErrExist):
		code = int(t.ErrorCodeAlreadyExists)
	case errors.Is(err, fs.ErrPermission):
		code = int(t.ErrorCodePermissionDenied)
	}
	if code != 0 {
		return &ahpRPCError{Code: code, Message: err.Error()}
	}
	return err
}
func (h *ahpHost) resource(req ahpRequest) (any, error) {
	var p struct {
		Channel        string            `json:"channel"`
		URI            string            `json:"uri"`
		Encoding       t.ContentEncoding `json:"encoding"`
		Write          bool              `json:"write"`
		FollowSymlinks *bool             `json:"followSymlinks"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.Channel != t.RootResourceURI {
		return nil, ahpInvalid("root channel required")
	}
	switch req.Method {
	case "resourceWrite", "resourceCopy", "resourceMove", "resourceDelete", "resourceMkdir":
		return h.mutateResource(req)
	}
	if p.Encoding != "" && p.Encoding != t.ContentEncodingUtf8 && p.Encoding != t.ContentEncodingBase64 {
		return nil, ahpInvalid("unsupported encoding")
	}
	if strings.HasPrefix(p.URI, "ahp-content:") {
		h.mu.Lock()
		content, exists := h.changesetContentLocked(p.URI)
		h.mu.Unlock()
		if !exists {
			return nil, &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "changeset content is no longer retained; refresh the changeset"}
		}
		switch req.Method {
		case "resourceRead":
			return resourceContent([]byte(content), p.Encoding), nil
		case "resourceResolve":
			return t.ResourceResolveResult{Uri: p.URI, Type: t.ResourceTypeFile, Size: ahpPtr(int64(len(content))), Etag: ahpPtr(fileRevision([]byte(content)))}, nil
		case "resourceRequest":
			if !p.Write {
				return struct{}{}, nil
			}
		}
		return nil, ahpInvalid("changeset content is read-only")
	}
	rel, err := h.resourcePath(p.URI)
	if err != nil {
		return nil, err
	}
	root := h.server.workspace.Root
	switch req.Method {
	case "resourceList":
		entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(rel))
		if err != nil {
			return nil, err
		}
		out := []t.DirectoryEntry{}
		for _, entry := range entries {
			kind := "file"
			if entry.IsDir() {
				kind = "directory"
			}
			out = append(out, t.DirectoryEntry{Name: entry.Name(), Type: kind})
		}
		return t.ResourceListResult{Entries: out}, nil
	case "resourceResolve":
		info, err := root.Lstat(rel)
		if p.FollowSymlinks == nil || *p.FollowSymlinks {
			info, err = root.Stat(rel)
			if err == nil {
				canonical, resolveErr := filepath.EvalSymlinks(filepath.Join(h.server.workspace.RootPath, rel))
				if resolveErr != nil {
					return nil, resolveErr
				}
				_, resolveErr = h.resourcePath(fileuri.FromPath(canonical))
				if resolveErr != nil {
					return nil, resolveErr
				}
				p.URI = fileuri.FromPath(canonical)
			}
		}
		if err != nil {
			return nil, err
		}
		kind := t.ResourceTypeFile
		if info.IsDir() {
			kind = t.ResourceTypeDirectory
		} else if info.Mode()&fs.ModeSymlink != 0 {
			kind = t.ResourceTypeSymlink
		}
		result := t.ResourceResolveResult{Uri: p.URI, Type: kind, Size: ahpPtr(info.Size()), Mtime: ahpPtr(info.ModTime().UTC().Format(time.RFC3339Nano))}
		if info.Mode().IsRegular() {
			if contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(rel))); contentType != "" {
				result.ContentType = &contentType
			}
		}
		if info.Mode().IsRegular() && info.Size() <= ahpResourceLimit {
			data, err := h.readResourceFile(rel)
			if err != nil {
				return nil, err
			}
			result.Etag, result.Size = ahpPtr(fileRevision(data)), ahpPtr(int64(len(data)))
			detected := http.DetectContentType(data)
			// Extensions can name both source files and media (.ts is also an
			// MPEG transport stream). Prefer actual text over a media guess,
			// while retaining structured text types such as JSON and SVG.
			if result.ContentType != nil && strings.HasPrefix(detected, "text/") && utf8.Valid(data) && !isBinary(data) {
				media := *result.ContentType
				if strings.HasPrefix(media, "video/") || strings.HasPrefix(media, "audio/") || strings.HasPrefix(media, "image/") && media != "image/svg+xml" {
					result.ContentType = &detected
				}
			}
			if result.ContentType == nil {
				result.ContentType = &detected
			}
		}
		return result, nil
	case "resourceRequest":
		if p.Write {
			if rel == "." {
				return nil, ahpInvalid("workspace root is not writable as a resource")
			}
			return struct{}{}, nil
		}
		_, err := root.Stat(rel)
		return struct{}{}, err
	case "resourceRead":
		data, err := h.readResourceFile(rel)
		if err != nil {
			return nil, err
		}
		return resourceContent(data, p.Encoding), nil
	}
	return nil, ahpInvalid(req.Method)
}
func resourceContent(data []byte, encoding t.ContentEncoding) t.ResourceReadResult {
	if encoding == "" {
		encoding = t.ContentEncodingUtf8
	}
	content := string(data)
	if encoding == t.ContentEncodingBase64 || !utf8.Valid(data) || isBinary(data) {
		encoding, content = t.ContentEncodingBase64, base64.StdEncoding.EncodeToString(data)
	}
	return t.ResourceReadResult{Data: content, Encoding: encoding, ContentType: ahpPtr(http.DetectContentType(data))}
}
func (h *ahpHost) readResourceFile(rel string) ([]byte, error) {
	// Opening a FIFO or device for reading can block, so check the type first.
	if info, err := h.server.workspace.Root.Stat(rel); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, ahpInvalid("resource must be a regular file")
	}
	f, err := h.server.workspace.Root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ahpInvalid("resource must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, ahpResourceLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > ahpResourceLimit {
		return nil, fmt.Errorf("resource exceeds %d bytes", ahpResourceLimit)
	}
	return data, nil
}

func (h *ahpHost) mutateResource(req ahpRequest) (any, error) {
	var p struct {
		t.ResourceWriteParams
		Source       string `json:"source"`
		Destination  string `json:"destination"`
		FailIfExists bool   `json:"failIfExists"`
		Recursive    bool   `json:"recursive"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return nil, ahpInvalid("invalid resource parameters")
	}
	uri := p.Uri
	if req.Method == "resourceCopy" || req.Method == "resourceMove" {
		uri = p.Source
	}
	rel, err := h.resourcePath(uri)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return nil, ahpInvalid("cannot mutate the workspace root")
	}
	s, root := h.server, h.server.workspace.Root
	s.fileWriteMu.Lock()
	defer s.fileWriteMu.Unlock()
	switch req.Method {
	case "resourceWrite":
		err = h.writeResource(rel, p.ResourceWriteParams)
	case "resourceMkdir":
		if info, statErr := root.Stat(rel); statErr == nil && !info.IsDir() {
			err = fs.ErrExist
		} else {
			err = root.MkdirAll(rel, 0755)
		}
	case "resourceDelete":
		if _, err = root.Lstat(rel); err == nil {
			if p.Recursive {
				err = root.RemoveAll(rel)
			} else {
				err = root.Remove(rel)
			}
		}
	case "resourceCopy", "resourceMove":
		var destination string
		destination, err = h.resourcePath(p.Destination)
		if err != nil {
			break
		}
		if destination == "." || destination == rel {
			return nil, ahpInvalid("source and destination must be distinct non-root resources")
		}
		var info fs.FileInfo
		info, err = root.Lstat(rel)
		if err != nil {
			break
		}
		// On a case-insensitive file system a case-only rename names the source itself.
		if req.Method == "resourceMove" && strings.EqualFold(rel, destination) {
			if same, statErr := root.Lstat(destination); statErr == nil && os.SameFile(info, same) {
				err = root.Rename(rel, destination)
				break
			}
		}
		// Resolve aliases before any overwrite. A symlink source can point
		// inside the destination that an overwrite would otherwise remove.
		canonicalSource, sourceErr := filepath.EvalSymlinks(filepath.Join(h.server.workspace.RootPath, rel))
		canonicalDestination, destinationErr := resourceDestinationPath(filepath.Join(h.server.workspace.RootPath, destination))
		if sourceErr == nil && destinationErr == nil && (canonicalSource == canonicalDestination || strings.HasPrefix(canonicalSource, canonicalDestination+string(filepath.Separator)) || info.IsDir() && strings.HasPrefix(canonicalDestination, canonicalSource+string(filepath.Separator))) {
			return nil, ahpInvalid("destination contains or aliases the source")
		}
		// Reject both lexical descendants and aliases into the source directory.
		if info.IsDir() {
			if strings.HasPrefix(destination, rel+string(filepath.Separator)) || strings.HasPrefix(rel, destination+string(filepath.Separator)) {
				return nil, ahpInvalid("cannot place a folder inside itself")
			}
			for parent := filepath.Dir(destination); ; parent = filepath.Dir(parent) {
				ancestor, statErr := root.Stat(parent)
				if statErr == nil && os.SameFile(info, ancestor) {
					return nil, ahpInvalid("cannot place a folder inside itself")
				}
				if parent == "." {
					break
				}
			}
		}
		if destinationInfo, statErr := root.Lstat(destination); statErr == nil {
			if os.SameFile(info, destinationInfo) {
				return nil, ahpInvalid("source and destination refer to the same resource")
			}
			if destinationInfo.IsDir() {
				for parent := filepath.Dir(rel); ; parent = filepath.Dir(parent) {
					ancestor, e := root.Stat(parent)
					if e == nil && os.SameFile(ancestor, destinationInfo) {
						return nil, ahpInvalid("destination contains the source")
					}
					if parent == "." {
						break
					}
				}
			}
			if p.FailIfExists {
				return nil, fs.ErrExist
			}
			// Rename replaces a file atomically, but not a directory.
			if req.Method == "resourceCopy" || info.IsDir() || destinationInfo.IsDir() {
				if err = root.RemoveAll(destination); err != nil {
					break
				}
			}
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return nil, statErr
		}
		if req.Method == "resourceMove" {
			err = root.Rename(rel, destination)
		} else {
			err = copyPathInRoot(root, rel, destination, info)
		}
	}
	if err != nil {
		return nil, err
	}
	s.flushFiles()
	s.refreshDiagnostics()
	return struct{}{}, nil
}
func (h *ahpHost) writeResource(rel string, p t.ResourceWriteParams) error {
	data := []byte(p.Data)
	switch p.Encoding {
	case t.ContentEncodingUtf8:
		if !utf8.Valid(data) {
			return ahpInvalid("invalid UTF-8")
		}
	case t.ContentEncodingBase64:
		var err error
		data, err = base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return ahpInvalid("invalid base64")
		}
	default:
		return ahpInvalid("resourceWrite requires utf-8 or base64 encoding")
	}
	if len(data) > ahpResourceLimit {
		return ahpInvalid("resource exceeds 16 MiB")
	}
	root := h.server.workspace.Root
	info, err := root.Lstat(rel)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if exists && !info.Mode().IsRegular() {
		return ahpInvalid("write target must be a regular file")
	}
	if exists && p.CreateOnly != nil && *p.CreateOnly {
		return fs.ErrExist
	}
	var previous []byte
	mode, position := t.ResourceWriteModeTruncate, int64(0)
	if p.Mode != nil {
		mode = *p.Mode
	}
	if p.Position != nil {
		position = *p.Position
	}
	if mode != t.ResourceWriteModeTruncate && mode != t.ResourceWriteModeAppend && mode != t.ResourceWriteModeInsert {
		return ahpInvalid("invalid write mode")
	}
	if position < 0 || position > ahpResourceLimit {
		return ahpInvalid("invalid write position")
	}
	if exists && (p.IfMatch != nil || mode != t.ResourceWriteModeTruncate || position != 0) {
		previous, err = h.readResourceFile(rel)
		if err != nil {
			return err
		}
	}
	if p.IfMatch != nil && (!exists || *p.IfMatch != fileRevision(previous)) {
		return &ahpRPCError{Code: -32011, Message: "file changed on disk"}
	}
	if position > int64(len(previous)) {
		return ahpInvalid("write position exceeds resource size")
	}
	if mode == t.ResourceWriteModeAppend {
		position = int64(len(previous)) - position
	}
	next := append(append([]byte{}, previous[:position]...), data...)
	if mode != t.ResourceWriteModeTruncate {
		next = append(next, previous[position:]...)
	}
	if len(next) > ahpResourceLimit {
		return ahpInvalid("resource exceeds 16 MiB")
	}
	flags := os.O_WRONLY | os.O_CREATE
	if !exists || p.CreateOnly != nil && *p.CreateOnly {
		flags |= os.O_EXCL
	}
	f, err := root.OpenFile(rel, flags, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteAt(next, 0); err != nil {
		return err
	}
	return f.Truncate(int64(len(next)))
}

// Resolve the existing parent of a prospective destination as well as its name.
func resourceDestinationPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return resolved, err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resourceDestinationPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}
