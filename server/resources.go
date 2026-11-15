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
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const resourceLimit = 16 << 20

// resourceFiles serves one directory tree as resources.
type resourceFiles struct {
	root *os.Root
	dir  string
}

type resourceParams struct {
	Channel        string                `json:"channel"`
	URI            string                `json:"uri"`
	Source         string                `json:"source"`
	Encoding       types.ContentEncoding `json:"encoding"`
	Write          bool                  `json:"write"`
	FollowSymlinks *bool                 `json:"followSymlinks"`
}

func (p *Project) files() resourceFiles {
	return resourceFiles{root: p.workspace.Root, dir: p.workspace.RootPath}
}

func (p *Project) resourcePath(uri string) (string, error) {
	if p.workspace == nil {
		return "", ahp.InvalidParams("workspace unavailable")
	}
	return p.files().path(uri)
}

// path names a resource relative to the tree.
func (f resourceFiles) path(uri string) (string, error) {
	path, err := ahp.FilePath(uri)
	if err != nil {
		return "", err
	}
	rel, ok := relative(f.dir, path)
	if !ok {
		// Links reach the same tree under other names, such as /var and /private/var on macOS.
		rel, ok = relative(canonicalPath(f.dir), canonicalPath(path))
	}
	if !ok {
		return "", ahp.InvalidParams("resource is outside this Wingman workspace")
	}
	return rel, nil
}
func (p *Project) validateDirectory(uri string) error {
	rel, err := p.resourcePath(uri)
	if err != nil {
		return err
	}
	if rel != "." {
		return ahp.InvalidParams("start wingman server in the directory you want to use")
	}
	return nil
}
func resourceError(err error) error {
	if err == nil {
		return nil
	}
	code := 0
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code = int(types.ErrorCodeNotFound)
	case errors.Is(err, fs.ErrExist):
		code = int(types.ErrorCodeAlreadyExists)
	case errors.Is(err, fs.ErrPermission):
		code = int(types.ErrorCodePermissionDenied)
	}
	if code != 0 {
		return &ahp.Error{Code: code, Message: err.Error()}
	}
	return err
}
func (p *Project) resource(req ahp.Request) (any, error) {
	var params resourceParams
	if json.Unmarshal(req.Params, &params) != nil || params.Channel != types.RootResourceURI {
		return nil, ahp.InvalidParams("root channel required")
	}
	switch req.Method {
	case "resourceWrite", "resourceCopy", "resourceMove", "resourceDelete", "resourceMkdir":
		return p.mutateResource(req)
	}
	if params.Encoding != "" && params.Encoding != types.ContentEncodingUtf8 && params.Encoding != types.ContentEncodingBase64 {
		return nil, ahp.InvalidParams("unsupported encoding")
	}
	if strings.HasPrefix(params.URI, "ahp-content:") {
		p.host.mu.Lock()
		content, exists := p.changesetContentLocked(params.URI)
		p.host.mu.Unlock()
		if !exists {
			return nil, &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "changeset content is no longer retained; refresh the changeset"}
		}
		switch req.Method {
		case "resourceRead":
			return resourceContent([]byte(content), params.Encoding), nil
		case "resourceResolve":
			return types.ResourceResolveResult{Uri: params.URI, Type: types.ResourceTypeFile, Size: new(int64(len(content))), Etag: new(fileRevision([]byte(content)))}, nil
		case "resourceRequest":
			if !params.Write {
				return struct{}{}, nil
			}
		}
		return nil, ahp.InvalidParams("changeset content is read-only")
	}
	rel, err := p.resourcePath(params.URI)
	if err != nil {
		return nil, err
	}
	return p.files().read(req.Method, params, rel)
}

// read answers the requests that leave the tree unchanged.
func (f resourceFiles) read(method string, p resourceParams, rel string) (any, error) {
	root := f.root
	switch method {
	case "resourceList":
		entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(rel))
		if err != nil {
			return nil, err
		}
		out := []types.DirectoryEntry{}
		for _, entry := range entries {
			kind := "file"
			if entry.IsDir() {
				kind = "directory"
			}
			out = append(out, types.DirectoryEntry{Name: entry.Name(), Type: kind})
		}
		return types.ResourceListResult{Entries: out}, nil
	case "resourceResolve":
		info, err := root.Lstat(rel)
		if p.FollowSymlinks == nil || *p.FollowSymlinks {
			info, err = root.Stat(rel)
			if err == nil {
				canonical, resolveErr := filepath.EvalSymlinks(filepath.Join(f.dir, rel))
				if resolveErr != nil {
					return nil, resolveErr
				}
				_, resolveErr = f.path(fileuri.FromPath(canonical))
				if resolveErr != nil {
					return nil, resolveErr
				}
				p.URI = fileuri.FromPath(canonical)
			}
		}
		if err != nil {
			return nil, err
		}
		kind := types.ResourceTypeFile
		if info.IsDir() {
			kind = types.ResourceTypeDirectory
		} else if info.Mode()&fs.ModeSymlink != 0 {
			kind = types.ResourceTypeSymlink
		}
		result := types.ResourceResolveResult{Uri: p.URI, Type: kind, Size: new(info.Size()), Mtime: new(info.ModTime().UTC().Format(time.RFC3339Nano))}
		if info.Mode().IsRegular() {
			if contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(rel))); contentType != "" {
				result.ContentType = &contentType
			}
		}
		if info.Mode().IsRegular() && info.Size() <= resourceLimit {
			data, err := f.readFile(rel)
			if err != nil {
				return nil, err
			}
			result.Etag, result.Size = new(fileRevision(data)), new(int64(len(data)))
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
				return nil, ahp.InvalidParams("workspace root is not writable as a resource")
			}
			return struct{}{}, nil
		}
		_, err := root.Stat(rel)
		return struct{}{}, err
	case "resourceRead":
		data, err := f.readFile(rel)
		if err != nil {
			return nil, err
		}
		return resourceContent(data, p.Encoding), nil
	}
	return nil, ahp.InvalidParams(method)
}
func resourceContent(data []byte, encoding types.ContentEncoding) types.ResourceReadResult {
	if encoding == "" {
		encoding = types.ContentEncodingUtf8
	}
	content := string(data)
	if encoding == types.ContentEncodingBase64 || !utf8.Valid(data) || isBinary(data) {
		encoding, content = types.ContentEncodingBase64, base64.StdEncoding.EncodeToString(data)
	}
	return types.ResourceReadResult{Data: content, Encoding: encoding, ContentType: new(http.DetectContentType(data))}
}
func (p *Project) readResourceFile(rel string) ([]byte, error) {
	return p.files().readFile(rel)
}

func (f resourceFiles) readFile(rel string) ([]byte, error) {
	// Opening a FIFO or device for reading can block, so check the type first.
	if info, err := f.root.Stat(rel); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, ahp.InvalidParams("resource must be a regular file")
	}
	file, err := f.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ahp.InvalidParams("resource must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, resourceLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > resourceLimit {
		return nil, fmt.Errorf("resource exceeds %d bytes", resourceLimit)
	}
	return data, nil
}

func (p *Project) mutateResource(req ahp.Request) (any, error) {
	var params struct {
		types.ResourceWriteParams
		Source       string `json:"source"`
		Destination  string `json:"destination"`
		FailIfExists bool   `json:"failIfExists"`
		Recursive    bool   `json:"recursive"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, ahp.InvalidParams("invalid resource parameters")
	}
	uri := params.Uri
	if req.Method == "resourceCopy" || req.Method == "resourceMove" {
		uri = params.Source
	}
	rel, err := p.resourcePath(uri)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return nil, ahp.InvalidParams("cannot mutate the workspace root")
	}
	s, root := p, p.workspace.Root
	s.fileWriteMu.Lock()
	defer s.fileWriteMu.Unlock()
	switch req.Method {
	case "resourceWrite":
		err = p.writeResource(rel, params.ResourceWriteParams)
	case "resourceMkdir":
		if info, statErr := root.Stat(rel); statErr == nil && !info.IsDir() {
			err = fs.ErrExist
		} else {
			err = root.MkdirAll(rel, 0755)
		}
	case "resourceDelete":
		if _, err = root.Lstat(rel); err == nil {
			if params.Recursive {
				err = root.RemoveAll(rel)
			} else {
				err = root.Remove(rel)
			}
		}
	case "resourceCopy", "resourceMove":
		var destination string
		destination, err = p.resourcePath(params.Destination)
		if err != nil {
			break
		}
		if destination == "." || destination == rel {
			return nil, ahp.InvalidParams("source and destination must be distinct non-root resources")
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
		canonicalSource, sourceErr := filepath.EvalSymlinks(filepath.Join(p.workspace.RootPath, rel))
		canonicalDestination, destinationErr := resourceDestinationPath(filepath.Join(p.workspace.RootPath, destination))
		if sourceErr == nil && destinationErr == nil && (canonicalSource == canonicalDestination || strings.HasPrefix(canonicalSource, canonicalDestination+string(filepath.Separator)) || info.IsDir() && strings.HasPrefix(canonicalDestination, canonicalSource+string(filepath.Separator))) {
			return nil, ahp.InvalidParams("destination contains or aliases the source")
		}
		// Reject both lexical descendants and aliases into the source directory.
		if info.IsDir() {
			if strings.HasPrefix(destination, rel+string(filepath.Separator)) || strings.HasPrefix(rel, destination+string(filepath.Separator)) {
				return nil, ahp.InvalidParams("cannot place a folder inside itself")
			}
			for parent := filepath.Dir(destination); ; parent = filepath.Dir(parent) {
				ancestor, statErr := root.Stat(parent)
				if statErr == nil && os.SameFile(info, ancestor) {
					return nil, ahp.InvalidParams("cannot place a folder inside itself")
				}
				if parent == "." {
					break
				}
			}
		}
		if destinationInfo, statErr := root.Lstat(destination); statErr == nil {
			if os.SameFile(info, destinationInfo) {
				return nil, ahp.InvalidParams("source and destination refer to the same resource")
			}
			if destinationInfo.IsDir() {
				for parent := filepath.Dir(rel); ; parent = filepath.Dir(parent) {
					ancestor, e := root.Stat(parent)
					if e == nil && os.SameFile(ancestor, destinationInfo) {
						return nil, ahp.InvalidParams("destination contains the source")
					}
					if parent == "." {
						break
					}
				}
			}
			if params.FailIfExists {
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
func (p *Project) writeResource(rel string, params types.ResourceWriteParams) error {
	data := []byte(params.Data)
	switch params.Encoding {
	case types.ContentEncodingUtf8:
		if !utf8.Valid(data) {
			return ahp.InvalidParams("invalid UTF-8")
		}
	case types.ContentEncodingBase64:
		var err error
		data, err = base64.StdEncoding.DecodeString(params.Data)
		if err != nil {
			return ahp.InvalidParams("invalid base64")
		}
	default:
		return ahp.InvalidParams("resourceWrite requires utf-8 or base64 encoding")
	}
	if len(data) > resourceLimit {
		return ahp.InvalidParams("resource exceeds 16 MiB")
	}
	root := p.workspace.Root
	info, err := root.Lstat(rel)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if exists && !info.Mode().IsRegular() {
		return ahp.InvalidParams("write target must be a regular file")
	}
	if exists && params.CreateOnly != nil && *params.CreateOnly {
		return fs.ErrExist
	}
	var previous []byte
	mode, position := types.ResourceWriteModeTruncate, int64(0)
	if params.Mode != nil {
		mode = *params.Mode
	}
	if params.Position != nil {
		position = *params.Position
	}
	if mode != types.ResourceWriteModeTruncate && mode != types.ResourceWriteModeAppend && mode != types.ResourceWriteModeInsert {
		return ahp.InvalidParams("invalid write mode")
	}
	if position < 0 || position > resourceLimit {
		return ahp.InvalidParams("invalid write position")
	}
	if exists && (params.IfMatch != nil || mode != types.ResourceWriteModeTruncate || position != 0) {
		previous, err = p.readResourceFile(rel)
		if err != nil {
			return err
		}
	}
	if params.IfMatch != nil && (!exists || *params.IfMatch != fileRevision(previous)) {
		return &ahp.Error{Code: -32011, Message: "file changed on disk"}
	}
	if position > int64(len(previous)) {
		return ahp.InvalidParams("write position exceeds resource size")
	}
	if mode == types.ResourceWriteModeAppend {
		position = int64(len(previous)) - position
	}
	next := append(append([]byte{}, previous[:position]...), data...)
	if mode != types.ResourceWriteModeTruncate {
		next = append(next, previous[position:]...)
	}
	if len(next) > resourceLimit {
		return ahp.InvalidParams("resource exceeds 16 MiB")
	}
	flags := os.O_WRONLY | os.O_CREATE
	if !exists || params.CreateOnly != nil && *params.CreateOnly {
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
