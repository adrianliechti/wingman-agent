package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

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

func (h *ahpHost) resource(req ahpRequest) (any, error) {
	var p struct {
		Channel        string            `json:"channel"`
		URI            string            `json:"uri"`
		Encoding       t.ContentEncoding `json:"encoding"`
		Write          bool              `json:"write"`
		FollowSymlinks *bool             `json:"followSymlinks"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.Channel != t.RootResourceURI {
		return nil, ahpInvalid("root channel and resource URI required")
	}
	if strings.HasPrefix(p.URI, "ahp-content:") {
		h.mu.Lock()
		content, exists := h.contents[p.URI]
		h.mu.Unlock()
		if !exists {
			return nil, &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "changeset content is no longer retained; refresh the changeset"}
		}
		switch req.Method {
		case "resourceRead":
			encoding := p.Encoding
			if encoding == "" {
				encoding = t.ContentEncodingUtf8
			}
			if encoding == t.ContentEncodingBase64 {
				content = base64.StdEncoding.EncodeToString([]byte(content))
			} else if encoding != t.ContentEncodingUtf8 {
				return nil, ahpInvalid("unsupported encoding")
			}
			return t.ResourceReadResult{Data: content, Encoding: encoding, ContentType: ahpPtr("text/plain")}, nil
		case "resourceResolve":
			return t.ResourceResolveResult{Uri: p.URI, Type: t.ResourceTypeFile, Size: ahpPtr(int64(len(content)))}, nil
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
		info, err := root.Stat(rel)
		if err != nil {
			return nil, err
		}
		kind := t.ResourceTypeFile
		if info.IsDir() {
			kind = t.ResourceTypeDirectory
		}
		mtime := info.ModTime().UTC().Format(time.RFC3339Nano)
		return t.ResourceResolveResult{Uri: p.URI, Type: kind, Size: ahpPtr(info.Size()), Mtime: &mtime}, nil
	case "resourceRequest":
		if p.Write {
			return nil, ahpInvalid("client file writes are not supported")
		}
		_, err := root.Stat(rel)
		return struct{}{}, err
	case "resourceRead":
		file, err := root.Open(rel)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, ahpInvalid("resource must be a regular file")
		}
		const limit = 16 << 20
		data, err := io.ReadAll(io.LimitReader(file, limit+1))
		if err != nil {
			return nil, err
		}
		if len(data) > limit {
			return nil, fmt.Errorf("resource exceeds %d bytes", limit)
		}
		encoding := p.Encoding
		if encoding == "" {
			encoding = t.ContentEncodingUtf8
		}
		content := string(data)
		if encoding == "base64" {
			content = base64.StdEncoding.EncodeToString(data)
		} else if encoding != "utf-8" {
			return nil, ahpInvalid("unsupported encoding")
		}
		contentType := http.DetectContentType(data)
		return t.ResourceReadResult{Data: content, Encoding: encoding, ContentType: &contentType}, nil
	}
	return nil, ahpInvalid(strings.TrimSpace(req.Method))
}
