package markdown

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

var fileLineSuffix = regexp.MustCompile(`:(\d+)(?::\d+)?$`)

func (r *ANSIRenderer) linkDestination(destination string) string {
	if destination == "" || strings.ContainsFunc(destination, unicode.IsControl) {
		return ""
	}
	u, err := url.Parse(destination)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return u.String()
	}
	if err == nil && u.Scheme == "file" && strings.HasPrefix(u.Path, "/") {
		return u.String()
	}
	// Relative paths may have a colon line suffix, which url.Parse interprets
	// as a scheme. Strip the suffix before recognizing local file references.
	path, fragment := splitFileReference(destination)
	if strings.Contains(path, "://") || strings.HasPrefix(path, "#") || path == "" {
		return ""
	}
	if path, err = url.PathUnescape(path); err != nil || strings.ContainsFunc(path, unicode.IsControl) {
		return ""
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		if r.options.Directory == "" || strings.Contains(path, ":") {
			return ""
		}
		path = filepath.Join(r.options.Directory, path)
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(path, "/") { // Windows drive path in a file URI.
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path, Fragment: fragment}).String()
}

func splitFileReference(reference string) (string, string) {
	path, fragment, _ := strings.Cut(reference, "#")
	if suffix := fileLineSuffix.FindStringSubmatchIndex(path); suffix != nil {
		fragment = "L" + path[suffix[2]:suffix[3]]
		path = path[:suffix[0]]
	}
	return path, fragment
}

// Inline code becomes a link only when it names an existing file. Ordinary
// code and commands keep their normal styling. Cache candidate lookups per render.
func (r *ANSIRenderer) codeFileDestination(text string) string {
	if r.fileLinks == nil {
		r.fileLinks = make(map[string]string)
	}
	if destination, ok := r.fileLinks[text]; ok {
		return destination
	}
	r.fileLinks[text] = ""
	path, _ := splitFileReference(text)
	if strings.ContainsAny(path, "\n\t") || !strings.ContainsAny(path, "/\\.") {
		return ""
	}
	destination := r.linkDestination(text)
	u, err := url.Parse(destination)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	local := filepath.FromSlash(u.Path)
	if len(local) > 2 && local[0] == '/' && local[2] == ':' {
		local = local[1:]
	}
	if info, err := os.Stat(local); err == nil && !info.IsDir() {
		r.fileLinks[text] = destination
		return destination
	}
	return ""
}
