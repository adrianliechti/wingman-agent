package fetch

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestFetchImagesByContent(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, tool.MaxImageDimension+1, 1))); err != nil {
		t.Fatal(err)
	}
	for _, contentType := range []string{"image/png", "application/octet-stream", "text/html"} {
		t.Run(contentType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect" {
					http.Redirect(w, r, "/image", http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", contentType)
				w.Write(buf.Bytes())
			}))
			defer server.Close()

			result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"url": server.URL + "/redirect"})
			if err != nil || !tool.IsImageResult(result.Content) {
				t.Fatalf("image = %.60s, err = %v", result.Content, err)
			}
			if result.Metadata["url"] != server.URL+"/image" || result.Metadata["width"] != tool.MaxImageDimension {
				t.Fatalf("metadata = %v", result.Metadata)
			}
		})
	}
}

func TestFetchImageUsesLargerSourceBudget(t *testing.T) {
	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 1500, 1500))); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= tool.MaxImageBytes {
		t.Fatal("fixture must exceed old fetch and attachment limits")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer server.Close()
	result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"url": server.URL})
	if err != nil || !tool.IsImageResult(result.Content) {
		t.Fatalf("large image = %.60s, err = %v", result.Content, err)
	}
}

func TestFetchImageSourceLimit(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprint("chunked=", chunked), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !chunked {
					w.Header().Set("Content-Length", fmt.Sprint(tool.MaxImageSourceBytes+1))
				}
				w.Write(buf.Bytes())
				if chunked {
					w.(http.Flusher).Flush()
				}
				io.CopyN(w, strings.NewReader(strings.Repeat("x", tool.MaxImageSourceBytes)), tool.MaxImageSourceBytes+1-int64(buf.Len()))
			}))
			defer server.Close()
			_, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"url": server.URL})
			if err == nil || !strings.Contains(err.Error(), "source limit") {
				t.Fatalf("oversized image error = %v", err)
			}
		})
	}
}
