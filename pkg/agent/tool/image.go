package tool

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
)

// MaxImageResultBytes bounds a data-URL tool result that may be forwarded to
// the model as an image attachment; larger payloads stay ordinary text and
// fall under normal output truncation.
const MaxImageResultBytes = 8 * 1024 * 1024

// MaxImageDimension matches the strictest input-image dimension accepted by
// supported model providers. Rejecting larger tool results keeps an otherwise
// valid image from poisoning the next model request.
const MaxImageDimension = 8000

var imageResultMimes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
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
	if err != nil {
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
