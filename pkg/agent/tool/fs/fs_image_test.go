package fs

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageToolViewsPNG(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	out, err := ImageTool(root).Execute(context.Background(), map[string]any{"file_path": "shot.png"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "data:image/png;base64,") {
		t.Fatalf("output = %q", out[:min(len(out), 60)])
	}
}

func TestImageToolRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if _, err := ImageTool(root).Execute(context.Background(), map[string]any{"file_path": "notes.txt"}); err == nil {
		t.Fatal("expected error for non-image file")
	}
}
