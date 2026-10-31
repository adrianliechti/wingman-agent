package remote

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Bug: when a paired browser aborts a streaming response, the relay only half-closes the yamux stream, so the workspace handler stays blocked in Write until yamux's 5-minute StreamCloseTimeout; the abort should reach the workspace promptly.
func TestReviewWorkspaceRelayAbortReachesWorkspaceHandler(t *testing.T) {
	relay := NewRelay("secret")
	server := httptest.NewServer(relay)
	t.Cleanup(func() { relay.Close(); server.Close() })
	var active, written atomic.Int64
	chunk := append(bytes.Repeat([]byte("x"), 32<<10), '\n')
	credentials := startRemoteHandler(t, server.URL, "secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		flusher := w.(http.Flusher)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			flusher.Flush()
			written.Add(int64(len(chunk)))
		}
	}))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	pairBrowser(t, client, server.URL, credentials)
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/stream", nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(res.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// Like a browser that stopped consuming, let every buffer fill first.
	for previous := int64(-1); previous != written.Load(); {
		previous = written.Load()
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	res.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if active.Load() != 0 {
		t.Fatal("workspace handler still writing 5s after the browser aborted")
	}
}

func TestReviewWorkspaceRelayStreamsAndForwardsOnlyPairedRequests(t *testing.T) {
	relay := NewRelay("secret")
	server := httptest.NewServer(relay)
	t.Cleanup(func() { relay.Close(); server.Close() })
	release := make(chan struct{})
	seen := make(chan http.Header, 16)
	credentials := startRemoteHandler(t, server.URL, "secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		if r.URL.Path == "/ndjson" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			io.WriteString(w, "{\"type\":\"file\"}\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			io.WriteString(w, "{\"type\":\"done\"}\n")
			return
		}
		io.WriteString(w, "ok")
	}))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	stranger := &http.Client{Timeout: 5 * time.Second}
	for _, cookie := range []string{"", cookieName + "=", cookieName + "=" + credentials.ID, cookieName + "=" + credentials.ID + "." + strings.Repeat("a", len(credentials.Key)), cookieName + "=" + credentials.ID + ".." + credentials.Key, cookieName + "=" + strings.ToUpper(credentials.ID) + "." + credentials.Key} {
		req, _ := http.NewRequest("GET", server.URL+"/ndjson", nil)
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		res, err := stranger.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Fatalf("unpaired cookie %q reached the workspace", cookie)
		}
	}
	select {
	case header := <-seen:
		t.Fatalf("workspace saw an unpaired request: %v", header)
	default:
	}
	pairBrowser(t, client, server.URL, credentials)
	req, _ := http.NewRequest("GET", server.URL+"/ndjson", nil)
	req.Header.Set("Cookie", "other=kept")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "{\"type\":\"file\"}\n" {
		t.Fatalf("first line before the handler finished: %q %v", first, err)
	}
	header := <-seen
	if strings.Contains(header.Get("Cookie"), cookieName) || !strings.Contains(header.Get("Cookie"), "other=kept") {
		t.Fatalf("forwarded cookies: %q", header.Get("Cookie"))
	}
	close(release)
	rest, _ := io.ReadAll(reader)
	if string(rest) != "{\"type\":\"done\"}\n" {
		t.Fatalf("rest: %q", rest)
	}
}
