package fetch

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/text"
	"golang.org/x/net/html/charset"
)

func readResponse(ctx context.Context, resp *http.Response) (tool.Result, error) {
	reader := bufio.NewReader(resp.Body)
	head, err := reader.Peek(512)
	if err != nil && err != io.EOF {
		return tool.Result{}, fmt.Errorf("read response: %w", err)
	}
	finalURL := resp.Request.URL.Redacted()
	if tool.ImageMIME(head) != "" {
		if resp.ContentLength > tool.MaxImageSourceBytes {
			return tool.Result{}, fmt.Errorf("image exceeds the %dMB source limit", tool.MaxImageSourceBytes/(1024*1024))
		}
		result, err := tool.ReadImage(ctx, reader)
		if err != nil {
			return tool.Result{}, err
		}
		result.Metadata["url"] = finalURL
		return result, nil
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxReadBytes+1))
	if err != nil {
		return tool.Result{}, fmt.Errorf("read response: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	bodyTruncated := len(body) > maxReadBytes
	if bodyTruncated {
		body = body[:maxReadBytes]
	}
	contentType := resp.Header.Get("Content-Type")
	if bodyTruncated && (fetchedDocumentData(body) || (isFetchedDocument(body, resp.Request.URL.Path, contentType) && !isHTML(contentType, body))) {
		return tool.Result{}, fmt.Errorf("document exceeds the %d MiB source limit; download it with exec/curl and use read", maxReadBytes/(1024*1024))
	}
	doc, err := extractFetchedDocument(ctx, body, resp.Request.URL.Path, contentType)
	if err != nil {
		return tool.Result{}, fmt.Errorf("extract document: %w", err)
	}
	metadata := map[string]any{"url": finalURL, "media_type": contentType}
	var content, warning string
	if doc != nil {
		content = doc.Markdown
		metadata["format"] = string(doc.Format)
		if len(doc.Metadata) > 0 {
			metadata["document"] = doc.Metadata
		}
		if len(doc.Attachments) > 0 {
			metadata["attachment_count"] = len(doc.Attachments)
		}
		if pages := doc.Metadata["pages_needing_ocr"]; pages != "" {
			warning = fmt.Sprintf("[PDF pages %s need OCR or visual inspection; extracted text may be incomplete.]", text.HeadBytes(pages, 240))
		}
	} else if isHTML(contentType, body) {
		// Use the final URL so relative links keep working after redirects.
		content, err = htmlToText(ctx, body, finalURL, contentType, bodyTruncated)
	} else {
		content, err = decodeFetchedText(body, contentType, bodyTruncated)
	}
	if err != nil {
		return tool.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		content = "[no readable text content in the fetched response]"
	}
	outputTruncated := len(content) > maxOutputBytes
	if outputTruncated {
		content = text.HeadLines(content, maxOutputBytes)
		content += fmt.Sprintf("\n\n[truncated at %dKB]", maxOutputBytes/1024)
	}
	if bodyTruncated {
		content += fmt.Sprintf("\n\n[truncated: response exceeded the %d MiB source limit; only the beginning was read]", maxReadBytes/(1024*1024))
	}
	if warning != "" {
		content += "\n\n" + warning
	}
	metadata["truncated"] = bodyTruncated || outputTruncated
	return tool.Result{Content: content, Metadata: metadata}, nil
}

func decodeFetchedText(body []byte, contentType string, truncated bool) (string, error) {
	_, params, _ := mime.ParseMediaType(contentType)
	label := params["charset"]
	if bytes.HasPrefix(body, []byte{0xff, 0xfe}) {
		label = "utf-16le"
	} else if bytes.HasPrefix(body, []byte{0xfe, 0xff}) {
		label = "utf-16be"
	}
	if label != "" && !strings.EqualFold(label, "utf-8") && !strings.EqualFold(label, "utf8") {
		reader, err := charset.NewReaderLabel(label, bytes.NewReader(body))
		if err != nil {
			return "", fmt.Errorf("decode text charset: %w", err)
		}
		body, err = io.ReadAll(reader)
		if err != nil {
			return "", fmt.Errorf("decode text: %w", err)
		}
	}
	body = bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})
	if truncated {
		// The source limit may cut the final UTF-8 code point in half.
		for i := max(0, len(body)-utf8.UTFMax+1); i < len(body); i++ {
			if utf8.RuneStart(body[i]) && !utf8.FullRune(body[i:]) {
				body = body[:i]
				break
			}
		}
	}
	if isBinary(body) || !utf8.Valid(body) {
		return "", fmt.Errorf("response appears to be binary or invalid UTF-8 (%s); only text, supported documents, and PNG, JPEG, GIF, WebP images are supported", contentType)
	}
	return string(body), nil
}

func isHTML(contentType string, body []byte) bool {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		return true
	}
	if mediaType != "" && mediaType != "text/plain" {
		return false
	}
	head := strings.ToLower(string(body[:min(len(body), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

func isBinary(body []byte) bool {
	for _, c := range body {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' && c != '\f' && c != '\b' {
			return true
		}
	}
	return false
}
