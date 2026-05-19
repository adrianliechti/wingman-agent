//go:build windows

package clipboard_test

import (
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/tui/clipboard"
)

func TestContentSupportsTextAndImagePayloads(t *testing.T) {
	image := "data:image/png;base64,abc"
	content := Content{Text: "hello", Image: &image}

	if content.Text != "hello" {
		t.Fatalf("Text = %q, want hello", content.Text)
	}
	if content.Image == nil || *content.Image != image {
		t.Fatalf("Image = %#v, want %q", content.Image, image)
	}
}
