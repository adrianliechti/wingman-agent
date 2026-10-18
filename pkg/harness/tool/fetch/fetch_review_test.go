package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestFetchValidatesBeforeApproval(t *testing.T) {
	for _, rawURL := range []string{"http:///path", "http://", "http://example.test:65536", "http://example.test:0"} {
		t.Run(rawURL, func(t *testing.T) {
			calls := 0
			tl := Tools(&tool.Elicitation{Confirm: func(context.Context, string) (bool, error) {
				calls++
				return false, nil
			}})[0]
			if _, err := tl.Execute(t.Context(), map[string]any{"url": rawURL}); err == nil || calls != 0 {
				t.Fatalf("invalid URL prompted: %d calls, %v", calls, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	touched := false
	tl := Tools(&tool.Elicitation{Confirm: func(context.Context, string) (bool, error) {
		touched = true
		return false, nil
	}})[0]
	_, err := tl.Execute(ctx, map[string]any{"url": "https://example.test"})
	if !errors.Is(err, context.Canceled) || touched {
		t.Fatalf("cancelled request prompted: %v, %v", touched, err)
	}
}

func TestFetchLimitsRedirects(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 20 {
			http.Redirect(w, r, "/next", http.StatusFound)
			return
		}
		fmt.Fprint(w, "redirect limit was bypassed")
	}))
	defer server.Close()
	_, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"url": server.URL})
	if err == nil || !strings.Contains(err.Error(), "redirect") || calls.Load() > 10 {
		t.Fatalf("redirect limit: %d requests, %v", calls.Load(), err)
	}
}

func TestFetchRedirectApprovalIncludesPort(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		fmt.Fprint(w, "private service")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	tl := Tools(&tool.Elicitation{Confirm: func(context.Context, string) (bool, error) { return true, nil }})[0]
	if _, err := tl.Execute(t.Context(), map[string]any{"url": source.URL}); err == nil || targetCalls.Load() != 0 {
		t.Fatalf("redirect reached unapproved port: %d requests, %v", targetCalls.Load(), err)
	}
	if _, err := tl.Execute(t.Context(), map[string]any{"url": target.URL}); err != nil {
		t.Fatal(err)
	}
	if result, err := tl.Execute(t.Context(), map[string]any{"url": source.URL}); err != nil || result.Content != "private service" {
		t.Fatalf("approved redirect = %+v, %v", result, err)
	}
}

func TestFetchDocumentFailuresAreExplicit(t *testing.T) {
	for _, body := range []string{"%PDF-1.4\nnot a valid document", string(buildTestPDF("complete prefix")) + strings.Repeat(" ", maxReadBytes)} {
		for _, contentType := range []string{"application/pdf", "text/html"} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				fmt.Fprint(w, body)
			}))
			_, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"url": server.URL + "/file.pdf"})
			server.Close()
			if err == nil {
				t.Fatal("invalid or incomplete document reported as successful text")
			}
		}
	}
}

func TestFetchCancellationCoversResponseAndExtraction(t *testing.T) {
	for _, prefix := range []string{strings.Repeat("x", 512), "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 504)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, prefix)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		_, err := Tools(nil)[0].Execute(ctx, map[string]any{"url": server.URL})
		cancel()
		server.Close()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("response read ignored cancellation: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := htmlToText(ctx, []byte("<p>hello</p>"), "https://example.com", "text/html", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("HTML extraction swallowed cancellation: %v", err)
	}
	if _, err := extractFetchedDocument(ctx, buildTestPDF("cancelled"), "/a.pdf", "application/pdf"); !errors.Is(err, context.Canceled) {
		t.Fatalf("document extraction swallowed cancellation: %v", err)
	}
}

func TestFetchGzipLimitsApplyAfterDecompression(t *testing.T) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write([]byte(strings.Repeat("€", maxReadBytes/3+1)))
	w.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(buf.Bytes())
	}))
	defer server.Close()
	result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"url": server.URL})
	if err != nil || !utf8.ValidString(result.Content) || len(result.Content) > tool.MaxInlineResultBytes || !strings.Contains(result.Content, "2 MiB") {
		t.Fatalf("decompressed result: %d bytes, %v", len(result.Content), err)
	}
}

func TestFetchRedactsCredentialsAndDoesNotForwardThemAcrossOrigins(t *testing.T) {
	var receivedAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, "ok")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("initial request lost credentials")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	var prompt string
	tl := Tools(&tool.Elicitation{Confirm: func(_ context.Context, message string) (bool, error) {
		prompt = message
		return true, nil
	}})[0]
	if _, err := tl.Execute(t.Context(), map[string]any{"url": target.URL}); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("user", "private-password")
	result, err := tl.Execute(t.Context(), map[string]any{"url": u.String()})
	if err != nil || result.Content != "ok" || receivedAuth != "" || strings.Contains(prompt, "private-password") {
		t.Fatalf("credentials leaked or redirect failed: auth=%q prompt=%q err=%v", receivedAuth, prompt, err)
	}

	// Transport/redirect errors wrapped in *url.Error must also redact credentials.
	unapproved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer unapproved.Close()
	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, unapproved.URL, http.StatusFound)
	}))
	defer redirectSource.Close()
	ru, err := url.Parse(redirectSource.URL)
	if err != nil {
		t.Fatal(err)
	}
	ru.User = url.UserPassword("user", "private-password")
	if _, err := tl.Execute(t.Context(), map[string]any{"url": ru.String()}); err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatalf("error leaked credentials: %v", err)
	}
}

func TestFetchReportsTruncatedEmptyContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat(" ", maxReadBytes+1)+"important content")
	}))
	defer server.Close()
	result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"url": server.URL})
	if err != nil || !strings.Contains(result.Content, "truncated") || !strings.Contains(result.Content, "2 MiB") {
		t.Fatalf("lost source truncation: %+v, %v", result, err)
	}
}

func TestFetchDecodesTextAndRejectsNonText(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, want string
		wantError                     bool
	}{
		{"latin1", "text/plain; charset=iso-8859-1", "caf\xe9", "café", false},
		{"utf16", "text/plain; charset=utf-16le", "\xff\xfec\x00a\x00f\x00\xe9\x00", "café", false},
		{"invalid UTF8", "application/octet-stream", "\xff\xfd\xfc", "", true},
		{"control bytes", "application/octet-stream", "\x01\x02\x03", "", true},
		{"late binary", "text/plain", strings.Repeat("x", 9000) + "\x00", "", true},
		{"HTML binary", "text/html", "<p>hello</p>\x00\x01\x02", "", true},
		{"HTML case", "Text/HTML; Charset=UTF-8", "<p>Hello <b>world</b></p>", "Hello **world**", false},
		{"HTML meta charset", "text/html", `<html><head><meta charset="iso-8859-1"></head><body><p>caf` + "\xe9" + `</p></body></html>`, "café", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"url": server.URL})
			if tc.wantError {
				if err == nil {
					t.Fatalf("non-text accepted: %.60q", result.Content)
				}
			} else if err != nil || result.Content != tc.want || !utf8.ValidString(result.Content) {
				t.Fatalf("text = %q, %v; want %q", result.Content, err, tc.want)
			}
		})
	}
}

func TestFetchDoesNotTreatRedirectOrPartialResponseAsComplete(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusNotModified, http.StatusPartialContent} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, "incomplete")
		}))
		_, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"url": server.URL})
		server.Close()
		if err == nil {
			t.Fatalf("HTTP %d was accepted as a complete fetch", status)
		}
	}
}
