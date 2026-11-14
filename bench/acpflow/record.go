package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const maxRecordedBodyBytes = 32 << 20

type recordedExchange struct {
	AtMS       int64           `json:"at_ms"`
	DurationMS int64           `json:"duration_ms"`
	Path       string          `json:"path"`
	Status     int             `json:"status"`
	Request    json.RawMessage `json:"request"`
	Response   string          `json:"response"`
}

// Reads the provider wire format rather than agent telemetry, so it works for every agent.
type requestRecorder struct {
	server *httptest.Server
	start  time.Time

	mu        sync.Mutex
	exchanges []recordedExchange
}

func newRequestRecorder(gateway string) (*requestRecorder, error) {
	target, err := url.Parse(gateway)
	if err != nil {
		return nil, err
	}
	recorder := &requestRecorder{start: time.Now()}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !isInferencePath(r.URL.Path) {
			proxy.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRecordedBodyBytes))
		r.Body.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		// Let the transport negotiate compression so the copy is plain text.
		r.Header.Del("Accept-Encoding")

		started := time.Now()
		tee := &teeResponseWriter{ResponseWriter: w, status: http.StatusOK}
		proxy.ServeHTTP(tee, r)

		exchange := recordedExchange{
			AtMS:       started.Sub(recorder.start).Milliseconds(),
			DurationMS: time.Since(started).Milliseconds(),
			Path:       r.URL.Path,
			Status:     tee.status,
			Response:   tee.body.String(),
		}
		if json.Valid(body) {
			exchange.Request = body
		}
		recorder.mu.Lock()
		recorder.exchanges = append(recorder.exchanges, exchange)
		recorder.mu.Unlock()
	}))
	return recorder, nil
}

func isInferencePath(path string) bool {
	return strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/messages") || strings.HasSuffix(path, "/chat/completions")
}

func (r *requestRecorder) URL() string { return r.server.URL }

func (r *requestRecorder) Close() { r.server.Close() }

func (r *requestRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.exchanges)
}

func (r *requestRecorder) snapshot() []recordedExchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedExchange(nil), r.exchanges...)
}

func (r *requestRecorder) writeJSONL(path string) error {
	if path == "" {
		return nil
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, exchange := range r.snapshot() {
		if err := encoder.Encode(exchange); err != nil {
			return err
		}
	}
	return nil
}

type teeResponseWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *teeResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *teeResponseWriter) Write(data []byte) (int, error) {
	if w.body.Len() < maxRecordedBodyBytes {
		w.body.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *teeResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
