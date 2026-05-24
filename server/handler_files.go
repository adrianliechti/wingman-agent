package server

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// excludedDirs is the set of directory names that the file browser and
// the search endpoint both skip (they create huge listings the user
// never wants in the picker).
var excludedDirs = map[string]bool{
	"node_modules": true,
	"__pycache__":  true,
	".venv":        true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"target":       true,
	".next":        true,
	".cache":       true,
}

var extToLanguage = map[string]string{
	".go":         "go",
	".js":         "javascript",
	".ts":         "typescript",
	".tsx":        "tsx",
	".jsx":        "jsx",
	".py":         "python",
	".rs":         "rust",
	".java":       "java",
	".kt":         "kotlin",
	".rb":         "ruby",
	".php":        "php",
	".c":          "c",
	".cpp":        "cpp",
	".h":          "c",
	".hpp":        "cpp",
	".cs":         "csharp",
	".swift":      "swift",
	".sh":         "bash",
	".bash":       "bash",
	".zsh":        "bash",
	".yaml":       "yaml",
	".yml":        "yaml",
	".json":       "json",
	".xml":        "xml",
	".html":       "html",
	".css":        "css",
	".scss":       "scss",
	".sql":        "sql",
	".md":         "markdown",
	".toml":       "toml",
	".ini":        "ini",
	".cfg":        "ini",
	".dockerfile": "dockerfile",
	".proto":      "protobuf",
	".lua":        "lua",
	".r":          "r",
	".dart":       "dart",
	".zig":        "zig",
	".ex":         "elixir",
	".exs":        "elixir",
	".erl":        "erlang",
	".hs":         "haskell",
	".ml":         "ocaml",
	".tf":         "hcl",
	".vue":        "vue",
	".svelte":     "svelte",
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	dirPath := r.URL.Query().Get("path")
	if dirPath == "" {
		dirPath = "."
	}

	dirPath = path.Clean(dirPath)
	if strings.HasPrefix(dirPath, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	fsys := s.workspace.Root.FS()

	entries, err := fs.ReadDir(fsys, dirPath)
	if err != nil {
		http.Error(w, "directory not found", http.StatusNotFound)
		return
	}

	var files []FileEntry

	for _, entry := range entries {
		name := entry.Name()

		if strings.HasPrefix(name, ".") {
			continue
		}

		if entry.IsDir() && excludedDirs[name] {
			continue
		}

		entryPath := path.Join(dirPath, name)
		if dirPath == "." {
			entryPath = name
		}

		var size int64
		if info, err := entry.Info(); err == nil {
			size = info.Size()
		}

		files = append(files, FileEntry{
			Name:  name,
			Path:  entryPath,
			IsDir: entry.IsDir(),
			Size:  size,
		})
	}

	if files == nil {
		files = []FileEntry{}
	}

	writeJSON(w, files)
}

func (s *Server) handleFilesSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	const limit = 50

	fsys := s.workspace.Root.FS()

	type hit struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}

	results := make([]hit, 0, limit)

	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		name := d.Name()

		if d.IsDir() {
			if p == "." {
				return nil
			}
			if strings.HasPrefix(name, ".") || excludedDirs[name] {
				return fs.SkipDir
			}
			return nil
		}

		if strings.HasPrefix(name, ".") {
			return nil
		}

		if q != "" && !strings.Contains(strings.ToLower(p), q) {
			return nil
		}

		results = append(results, hit{Path: p, Name: name})

		if len(results) >= limit {
			return fs.SkipAll
		}

		return nil
	})

	writeJSON(w, results)
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	filePath = path.Clean(filePath)
	if strings.HasPrefix(filePath, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	fsys := s.workspace.Root.FS()

	data, err := fs.ReadFile(fsys, filePath)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	size := int64(len(data))

	// SVG is XML, so the NUL-byte sniff classifies it as text — but visually
	// it's far more useful as a rendered image than as raw markup. Route it
	// to the binary preview path so the browser renders it via <img>.
	if strings.EqualFold(filepath.Ext(filePath), ".svg") {
		writeJSON(w, FileContent{
			Path:   filePath,
			Binary: true,
			Mime:   "image/svg+xml",
			Size:   size,
		})
		return
	}

	// Sniff for binary content. NUL bytes are the cleanest signal — any file
	// without them in the first 8KB is treated as text and opens in the
	// editor. JSON/XML stay editable; PNG/PDF/zip/etc. don't.
	if isBinary(data) {
		head := data
		if len(head) > 512 {
			head = head[:512]
		}
		mime := http.DetectContentType(head)
		writeJSON(w, FileContent{
			Path:   filePath,
			Binary: true,
			Mime:   mime,
			Size:   size,
		})
		return
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	lang := extToLanguage[ext]

	base := strings.ToLower(filepath.Base(filePath))
	if lang == "" {
		switch base {
		case "dockerfile":
			lang = "dockerfile"
		case "makefile":
			lang = "makefile"
		case "cmakelists.txt":
			lang = "cmake"
		}
	}

	writeJSON(w, FileContent{
		Path:     filePath,
		Content:  string(data),
		Language: lang,
		Size:     size,
	})
}

func isBinary(data []byte) bool {
	const sniff = 8192
	head := data
	if len(head) > sniff {
		head = head[:sniff]
	}
	return bytes.IndexByte(head, 0) >= 0
}

func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	abs, ok := s.resolveExistingRegularFile(w, body.Path)
	if !ok {
		return
	}
	info, _ := os.Lstat(abs) // already validated by resolveExistingRegularFile

	if err := os.WriteFile(abs, []byte(body.Content), info.Mode().Perm()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcast(Frame{Type: EvtFilesChanged})
	if s.workspace.Rewind != nil {
		s.broadcast(Frame{Type: EvtDiffsChanged})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resolveWorkspacePath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", false
	}
	return filepath.Join(s.workspace.RootPath, filepath.FromSlash(cleaned)), true
}

// resolveExistingRegularFile resolves a workspace-relative path and
// validates it points at an existing regular file (not a directory,
// not a symlink, not a device). Returns the absolute path or writes
// the appropriate HTTP error to w.
func (s *Server) resolveExistingRegularFile(w http.ResponseWriter, p string) (string, bool) {
	abs, ok := s.resolveWorkspacePath(p)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return "", false
	}
	// Lstat (not Stat): the resolveWorkspacePath check is lexical, so a
	// symlink inside the workspace could otherwise redirect the write/read
	// outside it. Rejecting non-regular files closes that hole.
	info, err := os.Lstat(abs)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
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
	return abs, true
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.resolveWorkspacePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if err := os.RemoveAll(abs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcast(Frame{Type: EvtFilesChanged})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	fromAbs, ok := s.resolveWorkspacePath(body.From)
	if !ok {
		http.Error(w, "invalid from path", http.StatusBadRequest)
		return
	}
	toAbs, ok := s.resolveWorkspacePath(body.To)
	if !ok {
		http.Error(w, "invalid to path", http.StatusBadRequest)
		return
	}

	if _, err := os.Lstat(toAbs); err == nil {
		http.Error(w, "destination already exists", http.StatusConflict)
		return
	}

	if err := os.Rename(fromAbs, toAbs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcast(Frame{Type: EvtFilesChanged})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFileCopy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	fromAbs, ok := s.resolveWorkspacePath(body.From)
	if !ok {
		http.Error(w, "invalid from path", http.StatusBadRequest)
		return
	}
	toAbs, ok := s.resolveWorkspacePath(body.To)
	if !ok {
		http.Error(w, "invalid to path", http.StatusBadRequest)
		return
	}

	if _, err := os.Lstat(toAbs); err == nil {
		http.Error(w, "destination already exists", http.StatusConflict)
		return
	}

	info, err := os.Lstat(fromAbs)
	if err != nil {
		http.Error(w, "source not found", http.StatusNotFound)
		return
	}

	if err := copyPath(fromAbs, toAbs, info); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcast(Frame{Type: EvtFilesChanged})
	w.WriteHeader(http.StatusNoContent)
}

func copyPath(src, dst string, info os.FileInfo) error {
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			ei, err := e.Info()
			if err != nil {
				return err
			}
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), ei); err != nil {
				return err
			}
		}
		return nil
	}

	// Symlinks must not be dereferenced — could escape the workspace.
	if !info.Mode().IsRegular() {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.resolveExistingRegularFile(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}

	name := filepath.Base(abs)
	disposition := "attachment; filename=\"" + strings.ReplaceAll(name, "\"", "") + "\"; filename*=UTF-8''" + url.PathEscape(name)
	w.Header().Set("Content-Disposition", disposition)
	http.ServeFile(w, r, abs)
}
