package tool

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

func pngDataURL(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestIsImageResult(t *testing.T) {
	valid := pngDataURL(t)

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid png", valid, true},
		{"plain text", "hello", false},
		{"invalid base64", "data:image/png;base64,!!!!", false},
		{"valid base64 but not an image", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("just some text bytes, definitely not a png")), false},
		{"mime mismatch", strings.Replace(valid, "image/png", "image/jpeg", 1), false},
		{"trailing newline", valid + "\n", false},
		{"embedded space", "data:image/png;base64,ab cd", false},
		{"unsupported mime", "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=", false},
		{"empty payload", "data:image/png;base64,", false},
		{"oversized", "data:image/png;base64," + strings.Repeat("A", MaxImageResultBytes), false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsImageResult(tt.input); got != tt.want {
				t.Fatalf("IsImageResult(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
