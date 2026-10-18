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

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestReadToolViewsImageByContent(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	for _, name := range []string{"shot.png", "no-extension", "misnamed.pdf"} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
			// A low text threshold must not send an image through line streaming.
			out, err := readTool(root, nil, 1).Execute(t.Context(), map[string]any{"file_path": name, "offset": 100, "limit": 1})
			if err != nil {
				t.Fatal(err)
			}
			if !tool.IsImageResult(out.Content) || out.Metadata["file_path"] != name {
				t.Fatalf("output = %.60s, metadata = %v", out.Content, out.Metadata)
			}
		})
	}
}

func TestReadToolTreatsImageExtensionWithTextAsText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.png"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	out, err := ReadTool(root).Execute(t.Context(), map[string]any{"file_path": "notes.png"})
	if err != nil || out.Content != "1\thello" {
		t.Fatalf("output = %q, err = %v", out.Content, err)
	}
}

func TestReadToolResizesOversizedImage(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, tool.MaxImageDimension+1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wide.png"), buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	out, err := ReadTool(root).Execute(context.Background(), map[string]any{"file_path": "wide.png"})
	if err != nil || !tool.IsImageResult(out.Content) {
		t.Fatalf("output = %.60s, error = %v", out.Content, err)
	}
	if out.Metadata["width"] != tool.MaxImageDimension {
		t.Fatalf("metadata = %v", out.Metadata)
	}
}

func TestReadToolImageSourceLimitAndReadRoots(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	outside := t.TempDir()
	path := filepath.Join(outside, "image.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTool(root).Execute(t.Context(), map[string]any{"file_path": path}); err == nil {
		t.Fatal("image outside read roots was accepted")
	}
	out, err := ReadTool(root, outside).Execute(t.Context(), map[string]any{"file_path": path})
	if err != nil || !tool.IsImageResult(out.Content) {
		t.Fatalf("allowed image = %.60s, error = %v", out.Content, err)
	}
	if err := os.Truncate(path, tool.MaxImageSourceBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTool(root, outside).Execute(t.Context(), map[string]any{"file_path": path}); err == nil || !strings.Contains(err.Error(), "source limit") {
		t.Fatalf("oversized image error = %v", err)
	}
}
