package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) resolveExistingRegularFile(w http.ResponseWriter, p string) (string, bool) {
	rel, info, ok := s.resolveExistingPath(w, p)
	if !ok {
		return "", false
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return "", false
	}
	if !info.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusBadRequest)
		return "", false
	}
	return rel, true
}

func (s *Server) resolveExistingPath(w http.ResponseWriter, p string) (string, os.FileInfo, bool) {
	rel, ok := workspaceRel(p)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return "", nil, false
	}
	info, err := s.Project.workspace.Root.Lstat(rel)
	if err != nil {
		http.Error(w, "path not found", http.StatusNotFound)
		return "", nil, false
	}
	return rel, info, true
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.resolveExistingRegularFile(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}

	root := s.Project.workspace.Root
	f, err := root.Open(rel)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	name := filepath.Base(rel)
	disposition := "attachment; filename=\"" + strings.ReplaceAll(name, "\"", "") + "\"; filename*=UTF-8''" + url.PathEscape(name)
	w.Header().Set("Content-Disposition", disposition)
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// handleFilePreview redirects an HTML document to the isolated preview server.
// The preview origin is rooted at the document's directory so both relative
// and root-relative asset URLs behave like they do on a normal static server.
func (s *Server) handleFilePreview(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.resolveExistingRegularFile(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, s.preview.startURL(rel, r.Host), http.StatusTemporaryRedirect)
}

type fileSearchHit struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// handleFileSearch serves the file picker: up to 50 visible files whose path
// contains the query.
func (s *Server) handleFileSearch(w http.ResponseWriter, r *http.Request) {
	hits := []fileSearchHit{}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	visited := 0
	err := fs.WalkDir(s.Project.workspace.Root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > 100000 {
			return fs.SkipAll
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(name, ".") || excludedDirs[name]) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || query != "" && !strings.Contains(strings.ToLower(path), query) {
			return nil
		}
		hits = append(hits, fileSearchHit{Path: path, Name: name})
		if len(hits) >= 50 {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, hits)
}

// handleReveal shows a workspace file or folder in the host's file manager.
func (s *Server) handleReveal(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		http.Error(w, "invalid reveal request", http.StatusBadRequest)
		return
	}
	rel, info, ok := s.resolveExistingPath(w, request.Path)
	if !ok {
		return
	}
	if err := revealPath(filepath.Join(s.Project.workspace.RootPath, rel), info.IsDir()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
