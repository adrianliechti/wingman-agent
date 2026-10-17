package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestReadImagePreservesSupportedFormats(t *testing.T) {
	for _, mime := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		t.Run(mime, func(t *testing.T) {
			var buf bytes.Buffer
			var err error
			switch mime {
			case "image/png":
				err = png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 1)))
			case "image/jpeg":
				err = jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 1)), nil)
			case "image/gif":
				frame := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{color.Black, color.White})
				err = gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{1, 1}})
			case "image/webp":
				data, decodeErr := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
				err = decodeErr
				buf.Write(data)
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := ReadImage(t.Context(), bytes.NewReader(buf.Bytes()))
			want := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
			if err != nil || result.Content != want || !IsImageResult(result.Content) {
				t.Fatalf("image was not preserved: %.60s, err = %v", result.Content, err)
			}
			// Trailing metadata/padding can exceed the byte budget even for a
			// tiny bitmap. Exercise each decoder and conversion to PNG.
			large := append(bytes.Clone(buf.Bytes()), make([]byte, MaxImageBytes-buf.Len()+1)...)
			result, err = ReadImage(t.Context(), bytes.NewReader(large))
			if err != nil || !IsImageResult(result.Content) || result.Metadata["media_type"] != "image/png" {
				t.Fatalf("image was not reencoded: %.60s, err = %v", result.Content, err)
			}
		})
	}
}

func TestReadImageResizesDimensionsAndPreservesTransparency(t *testing.T) {
	for _, size := range []image.Point{{MaxImageDimension + 2, 4}, {4, MaxImageDimension + 2}} {
		var buf bytes.Buffer
		if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, size.X, size.Y))); err != nil {
			t.Fatal(err)
		}
		result, err := ReadImage(t.Context(), &buf)
		if err != nil || !IsImageResult(result.Content) {
			t.Fatalf("resized image = %.60s, err = %v", result.Content, err)
		}
		_, encoded, _ := strings.Cut(result.Content, ";base64,")
		data, _ := base64.StdEncoding.DecodeString(encoded)
		bitmap, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		bounds := bitmap.Bounds()
		if max(bounds.Dx(), bounds.Dy()) != MaxImageDimension || min(bounds.Dx(), bounds.Dy()) != 3 {
			t.Fatalf("resized dimensions = %v", bounds)
		}
		if _, _, _, alpha := bitmap.At(0, 0).RGBA(); alpha != 0 {
			t.Fatalf("transparency lost: alpha = %d", alpha)
		}
	}
}

func TestReadImageReducesOversizedPayload(t *testing.T) {
	bitmap := image.NewNRGBA(image.Rect(0, 0, 2048, 1024))
	random := rand.New(rand.NewPCG(1, 2))
	for i := range bitmap.Pix {
		bitmap.Pix[i] = byte(random.Uint32())
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, bitmap); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= MaxImageBytes {
		t.Fatal("fixture must exceed attachment byte budget")
	}
	result, err := ReadImage(t.Context(), &buf)
	if err != nil || !IsImageResult(result.Content) {
		t.Fatalf("resized image = %.60s, err = %v", result.Content, err)
	}
	width, height := result.Metadata["width"].(int), result.Metadata["height"].(int)
	if width >= 2048 || width != height*2 {
		t.Fatalf("resized dimensions = %dx%d", width, height)
	}
}

func TestReadImageRejectsInvalidAndExcessiveSources(t *testing.T) {
	_, encoded, _ := strings.Cut(pngDataURL(t), ";base64,")
	huge, _ := base64.StdEncoding.DecodeString(encoded)
	binary.BigEndian.PutUint32(huge[16:20], 100000)
	binary.BigEndian.PutUint32(huge[20:24], 100000)
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"unsupported", []byte("not an image"), "unsupported image"},
		{"truncated", huge[:16], "read image"},
		{"pixel bomb", huge, "megapixel limit"},
		{"source bytes", make([]byte, MaxImageSourceBytes+1), "source limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadImage(t.Context(), bytes.NewReader(tc.data)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadImage(ctx, bytes.NewReader(huge)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read error = %v", err)
	}
}

func TestReadImageRejectsTruncatedBitmapAfterValidHeader(t *testing.T) {
	_, encoded, _ := strings.Cut(pngDataURL(t), ";base64,")
	data, _ := base64.StdEncoding.DecodeString(encoded)
	// PNG's dimensions remain readable after its pixel data is removed.
	header := data[:33]
	if _, _, err := ImageDimensions(header, "image/png"); err != nil {
		t.Fatalf("fixture has no valid header: %v", err)
	}
	if result, err := ReadImage(t.Context(), bytes.NewReader(header)); err == nil {
		t.Fatalf("corrupt image was attached: %.60s", result.Content)
	}
}
