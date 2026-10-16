package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/adrianliechti/go-extract"
)

// htmlToText converts a fetched page to Markdown. pageURL resolves relative
// links and contentType carries the response charset. Conversion failures fall
// back to the raw source so a malformed page never breaks fetching.
func htmlToText(ctx context.Context, src []byte, pageURL, contentType string, truncated bool) (string, error) {
	if bytes.HasPrefix(src, []byte{0xff, 0xfe}) || bytes.HasPrefix(src, []byte{0xfe, 0xff}) {
		decoded, err := decodeFetchedText(src, contentType, truncated)
		if err != nil {
			return "", err
		}
		src = []byte(decoded)
		contentType = "text/html; charset=utf-8"
	} else {
		if truncated {
			for i := max(0, len(src)-utf8.UTFMax+1); i < len(src); i++ {
				if utf8.RuneStart(src[i]) && !utf8.FullRune(src[i:]) {
					src = src[:i]
					break
				}
			}
		}
		if isBinary(src) {
			return "", fmt.Errorf("response appears to be binary or invalid UTF-8 (%s); only text, supported documents, and PNG, JPEG, GIF, WebP images are supported", contentType)
		}
	}

	doc, err := extract.Extract(ctx, extract.Input{
		MediaType: contentType,
		Data:      src,
	}, extract.Options{
		HTML: extract.HTMLOptions{BaseURL: pageURL},
	})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || doc == nil || doc.Format != extract.FormatHTML {
		return decodeFetchedText(src, contentType, truncated)
	}
	if isBinary([]byte(doc.Markdown)) || !utf8.ValidString(doc.Markdown) {
		return "", fmt.Errorf("response appears to be binary or invalid UTF-8 (%s); only text, supported documents, and PNG, JPEG, GIF, WebP images are supported", contentType)
	}

	return strings.TrimSpace(doc.Markdown), nil
}

func extractFetchedDocument(ctx context.Context, body []byte, urlPath, contentType string) (*extract.Document, error) {
	if !isFetchedDocument(body, urlPath, contentType) {
		return nil, nil
	}
	doc, err := extract.Extract(ctx, extract.Input{
		Name:      path.Base(urlPath),
		MediaType: contentType,
		Data:      body,
	}, extract.Options{DiscardAttachmentData: true})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, extract.ErrUnsupportedFormat) && !isFetchedDocument(body, "", contentType) {
		// A filename alone is a hint; a .pdf URL can serve ordinary text.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if doc == nil || doc.Format == extract.FormatText || doc.Format == extract.FormatMarkdown || doc.Format == extract.FormatHTML {
		return nil, nil
	}
	return doc, nil
}

func fetchedDocumentData(data []byte) bool {
	return bytes.HasPrefix(data, []byte("%PDF-")) ||
		bytes.HasPrefix(data, []byte{'P', 'K', 0x03, 0x04}) ||
		bytes.HasPrefix(data, []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}) ||
		(len(data) >= 5 && strings.EqualFold(string(data[:5]), `{\rtf`))
}

func isFetchedDocument(data []byte, urlPath, contentType string) bool {
	if fetchedDocumentData(data) {
		return true
	}
	if isHTML(contentType, data) {
		return false
	}
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "application/pdf") ||
		strings.Contains(ct, "application/rtf") ||
		strings.Contains(ct, "text/rtf") ||
		strings.Contains(ct, "officedocument") ||
		strings.Contains(ct, "application/vnd.ms-") ||
		strings.Contains(ct, "message/rfc822") {
		return true
	}
	switch strings.ToLower(path.Ext(urlPath)) {
	case ".pdf", ".rtf",
		".docx", ".docm", ".dotx", ".dotm",
		".xlsx", ".xlsm", ".xltx", ".xltm",
		".pptx", ".pptm", ".ppsx", ".ppsm", ".potx", ".potm",
		".eml", ".msg":
		return true
	default:
		return false
	}
}
