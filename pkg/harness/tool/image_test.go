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
	return pngSizeDataURL(t, 1, 1)
}

func pngSizeDataURL(t *testing.T, width, height int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
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
		{"oversized dimension", pngSizeDataURL(t, MaxImageDimension+1, 1), false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsImageResult(tt.input); got != tt.want {
				t.Fatalf("IsImageResult(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestImageDimensionsWebPVP8X(t *testing.T) {
	data := make([]byte, 30)
	copy(data[:4], "RIFF")
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8X")
	// VP8X stores canvas dimensions minus one as little-endian 24-bit values.
	data[24], data[25] = 0x3f, 0x1f // 8000 - 1
	data[27] = 0x08                 // 9 - 1

	width, height, err := ImageDimensions(data, "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	if width != 8000 || height != 9 {
		t.Fatalf("dimensions = %dx%d, want 8000x9", width, height)
	}
}
