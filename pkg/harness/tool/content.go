package tool

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Content is an ordered sequence of tool output blocks. Text stays text even
// when it looks like a data URL; images are explicit binary attachments.
//
// The order is part of the contract. A result with images reaches the model as
// one tool output made of text and image parts in this order, so an image
// stays attached to the call that produced it. Text-only content is sent as a
// single string. Anything a tool cannot express as text or an image should be
// described in text rather than omitted.
type Content []ContentBlock

// ContentBlock has exactly one of Text or Image set. A pointer distinguishes
// an empty text block from an absent one.
type ContentBlock struct {
	Text  *string       `json:"text,omitempty"`
	Image *ImageContent `json:"image,omitempty"`
}

type ImageContent struct {
	MIMEType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

func TextBlock(text string) ContentBlock { return ContentBlock{Text: &text} }
func TextContent(text string) Content    { return Content{TextBlock(text)} }

// Text returns the text projection for text-only consumers such as hooks and
// terminal summaries. It never embeds image bytes into text.
func (c Content) Text() string {
	var parts []string
	for _, block := range c {
		if block.Text != nil {
			parts = append(parts, *block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (c Content) String() string { return c.Text() }

// AppendText extends a trailing text block or appends one after an image.
func (c *Content) AppendText(text string) {
	if n := len(*c); n > 0 && (*c)[n-1].Text != nil {
		joined := *(*c)[n-1].Text + text
		(*c)[n-1] = TextBlock(joined)
	} else {
		*c = append(*c, TextBlock(text))
	}
}

func (c Content) Clone() Content {
	out := slices.Clone(c)
	for i, b := range c {
		if b.Text != nil {
			out[i].Text = new(string)
			*out[i].Text = *b.Text
		}
		if b.Image != nil {
			img := *b.Image
			img.Data = slices.Clone(img.Data)
			out[i].Image = &img
		}
	}
	return out
}

func (c Content) Validate() error {
	for i, block := range c {
		if (block.Text == nil) == (block.Image == nil) {
			return fmt.Errorf("content block %d must contain exactly one of text or image", i)
		}
		if block.Image != nil && !block.Image.Valid() {
			return fmt.Errorf("content block %d contains an invalid or oversized image", i)
		}
	}
	return nil
}

func (i ImageContent) DataURL() string {
	return "data:" + i.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(i.Data)
}

func (i ImageContent) Valid() bool {
	if len(i.Data) == 0 || len(i.Data) > MaxImageBytes || ImageMIME(i.Data) != i.MIMEType {
		return false
	}
	width, height, err := ImageDimensions(i.Data, i.MIMEType)
	return err == nil && width <= MaxImageDimension && height <= MaxImageDimension
}

// UnmarshalJSON reads both typed content and the text field in older journals.
// Legacy image attachments are migrated separately from their adjacent File.
func (c *Content) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		*c = TextContent(text)
		return nil
	}
	type plain Content
	var blocks plain
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return err
	}
	decoded := Content(blocks)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*c = decoded
	return nil
}

// WithText replaces the text projection while preserving image attachments.
// Unchanged text keeps the original block order. A rewrite occupies the first
// text block's position; hooks that replace the entire result use TextContent.
func (c Content) WithText(text string) Content {
	if c.Text() == text {
		return c
	}
	out := make(Content, 0, len(c)+1)
	written := false
	for _, b := range c {
		if b.Text != nil {
			if !written {
				out = append(out, TextBlock(text))
				written = true
			}
		} else {
			out = append(out, b)
		}
	}
	if !written {
		out = append(out, TextBlock(text))
	}
	return out
}

// ParseImageDataURL is for legacy journals and transport adapters only.
func ParseImageDataURL(url string) (*ImageContent, bool) {
	if len(url) > MaxImageResultBytes || strings.ContainsAny(url, " \t\r\n") {
		return nil, false
	}
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return nil, false
	}
	mime, encoded, ok := strings.Cut(rest, ";base64,")
	if !ok || !imageResultMimes[mime] || encoded == "" {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	img := &ImageContent{MIMEType: mime, Data: data}
	return img, img.Valid()
}

// ImageURLs encodes validated images at text-based transport boundaries.
func (c Content) ImageURLs() []string {
	var urls []string
	for _, b := range c {
		if b.Image != nil && b.Image.Valid() {
			urls = append(urls, b.Image.DataURL())
		}
	}
	return urls
}
