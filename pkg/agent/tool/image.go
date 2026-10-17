package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // Register WebP decoding for image resizing.
)

// Source images may be larger than the attachments accepted by providers.
const MaxImageSourceBytes = 32 * 1024 * 1024

const maxImagePixels = 64_000_000

// MaxImageBytes retains the existing 5MB attachment budget across providers.
const MaxImageBytes = 5 * 1024 * 1024

// MaxImageResultBytes bounds a data-URL tool result that may be forwarded to
// the model as an image attachment; larger payloads stay ordinary text and
// fall under normal output truncation.
const MaxImageResultBytes = 8 * 1024 * 1024

// MaxImageDimension matches the strictest input-image dimension accepted by
// supported model providers. Larger source images are resized before attachment.
const MaxImageDimension = 8000

var imageResultMimes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// ImageMIME identifies supported raster images by their contents, independent
// of file extensions or HTTP Content-Type headers. SVG remains text.
func ImageMIME(data []byte) string {
	mime := http.DetectContentType(data)
	if imageResultMimes[mime] {
		return mime
	}
	return ""
}

// ReadImage bounds source reads and returns an image data URL. Images that fit
// the attachment limits retain their bytes (including animation). Larger images
// become a PNG, preserving transparency and aspect ratio. GIF resizing uses the
// first frame; animated WebP must already fit the attachment limits.
func ReadImage(ctx context.Context, source io.Reader) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	data, err := io.ReadAll(io.LimitReader(source, MaxImageSourceBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read image: %w", err)
	}
	if len(data) > MaxImageSourceBytes {
		return Result{}, fmt.Errorf("image exceeds the %dMB source limit", MaxImageSourceBytes/(1024*1024))
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	mime := ImageMIME(data)
	width, height, err := ImageDimensions(data, mime)
	if err != nil {
		return Result{}, fmt.Errorf("read image: %w", err)
	}
	if int64(width) > maxImagePixels/int64(height) {
		return Result{}, fmt.Errorf("image is %dx%d; exceeds the %d megapixel limit; resize or crop it first", width, height, maxImagePixels/1_000_000)
	}
	originalWidth, originalHeight := width, height
	needsResize := len(data) > MaxImageBytes || width > MaxImageDimension || height > MaxImageDimension
	animatedWebP := mime == "image/webp" && len(data) >= 30 && string(data[12:16]) == "VP8X" && data[20]&2 != 0
	if animatedWebP && needsResize {
		return Result{}, fmt.Errorf("animated WebP exceeds attachment limits; resize it or extract a frame first")
	}
	var bitmap image.Image
	if !animatedWebP {
		// Decode even when preserving the bytes: a valid dimensions header
		// alone does not make a truncated or corrupt bitmap safe to attach.
		bitmap, _, err = image.Decode(bytes.NewReader(data))
		if err != nil {
			return Result{}, fmt.Errorf("decode image: %w", err)
		}
	}
	if needsResize {
		// A GIF's first frame may cover only part of its logical canvas.
		canvas := image.Rect(0, 0, width, height)
		if bitmap.Bounds() != canvas {
			composite := image.NewNRGBA(canvas)
			draw.Draw(composite, bitmap.Bounds(), bitmap, bitmap.Bounds().Min, draw.Src)
			bitmap = composite
		}
		if width > MaxImageDimension || height > MaxImageDimension {
			longest := max(width, height)
			width = max(1, int(int64(width)*MaxImageDimension/int64(longest)))
			height = max(1, int(int64(height)*MaxImageDimension/int64(longest)))
		}
		for {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			resized := image.NewNRGBA(image.Rect(0, 0, width, height))
			draw.ApproxBiLinear.Scale(resized, resized.Bounds(), bitmap, bitmap.Bounds(), draw.Src, nil)
			var encoded bytes.Buffer
			if err := png.Encode(&encoded, resized); err != nil {
				return Result{}, fmt.Errorf("encode resized image: %w", err)
			}
			data, mime = encoded.Bytes(), "image/png"
			if len(data) <= MaxImageBytes {
				break
			}
			// Keep the original aspect ratio across successive reductions.
			longest := max(width, height) * 3 / 4
			width = max(1, int(int64(originalWidth)*int64(longest)/int64(max(originalWidth, originalHeight))))
			height = max(1, int(int64(originalHeight)*int64(longest)/int64(max(originalWidth, originalHeight))))
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{
		Content: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
		Metadata: map[string]any{
			"media_type": mime, "width": width, "height": height,
			"original_width": originalWidth, "original_height": originalHeight,
		},
	}, nil
}

// IsImageResult reports whether a tool result is a well-formed image data URL
// safe to send as an input image: declared type matches the decoded bytes, so
// a tool echoing an arbitrary data:-prefixed string cannot poison the request.
func IsImageResult(s string) bool {
	if len(s) > MaxImageResultBytes || strings.ContainsAny(s, " \t\r\n") {
		return false
	}

	rest, ok := strings.CutPrefix(s, "data:")
	if !ok {
		return false
	}

	mime, encoded, ok := strings.Cut(rest, ";base64,")
	if !ok || !imageResultMimes[mime] || encoded == "" {
		return false
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) > MaxImageBytes {
		return false
	}

	if http.DetectContentType(decoded) != mime {
		return false
	}
	width, height, err := ImageDimensions(decoded, mime)
	return err == nil && width <= MaxImageDimension && height <= MaxImageDimension
}

// ImageDimensions reads image dimensions without decoding the full bitmap.
func ImageDimensions(data []byte, mime string) (int, int, error) {
	var width, height int
	switch mime {
	case "image/png":
		config, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, 0, err
		}
		width, height = config.Width, config.Height
	case "image/jpeg":
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, 0, err
		}
		width, height = config.Width, config.Height
	case "image/gif":
		config, err := gif.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, 0, err
		}
		width, height = config.Width, config.Height
	case "image/webp":
		return webPDimensions(data)
	default:
		return 0, 0, fmt.Errorf("unsupported image format %q", mime)
	}
	if width < 1 || height < 1 {
		return 0, 0, fmt.Errorf("invalid image dimensions %dx%d", width, height)
	}
	return width, height, nil
}

func webPDimensions(data []byte) (int, int, error) {
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, fmt.Errorf("invalid WebP header")
	}

	var width, height int
	switch string(data[12:16]) {
	case "VP8X":
		if len(data) < 30 {
			return 0, 0, fmt.Errorf("truncated VP8X header")
		}
		width = 1 + int(data[24]) + int(data[25])<<8 + int(data[26])<<16
		height = 1 + int(data[27]) + int(data[28])<<8 + int(data[29])<<16
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, fmt.Errorf("invalid VP8L header")
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		width = 1 + int(bits&0x3fff)
		height = 1 + int((bits>>14)&0x3fff)
	case "VP8 ":
		if len(data) < 30 || !bytes.Equal(data[23:26], []byte{0x9d, 0x01, 0x2a}) {
			return 0, 0, fmt.Errorf("invalid VP8 header")
		}
		width = int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
		height = int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
	default:
		return 0, 0, fmt.Errorf("unsupported WebP chunk %q", data[12:16])
	}
	if width < 1 || height < 1 {
		return 0, 0, fmt.Errorf("invalid image dimensions %dx%d", width, height)
	}
	return width, height, nil
}
