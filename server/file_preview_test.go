package server

import (
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

func TestFilePreviewConcurrentClose(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	preview, err := newFilePreviewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(preview.Close)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get(preview.URL())
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	var closers sync.WaitGroup
	for range 16 {
		closers.Go(preview.Close)
	}
	closers.Wait()
	select {
	case <-preview.done:
	default:
		t.Fatal("Close returned before the preview server stopped")
	}
	if response, err := client.Get(preview.URL()); err == nil {
		_ = response.Body.Close()
		t.Fatal("preview still accepted a connection after Close")
	}
}
