package tool

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// MaxImageResultBytes bounds a data-URL tool result that may be forwarded to
// the model as an image attachment; larger payloads stay ordinary text and
// fall under normal output truncation.
const MaxImageResultBytes = 8 * 1024 * 1024

var imageResultMimes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// IsImageResult reports whether a tool result is a well-formed image data URL
// safe to send as an input image: declared type matches the decoded bytes, so
// a tool echoing an arbitrary data:-prefixed string cannot poison the request.
func IsImageResult(s string) bool {
	if len(s) > MaxImageResultBytes || strings.ContainsAny(s, " \t\r\n") {
		return false
	}

	rest, ok := strings.CutPrefix(s, "data:")
	if !ok {
		return false
	}

	mime, encoded, ok := strings.Cut(rest, ";base64,")
	if !ok || !imageResultMimes[mime] || encoded == "" {
		return false
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}

	return http.DetectContentType(decoded) == mime
}
