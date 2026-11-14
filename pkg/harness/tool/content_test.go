package tool

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestContentJSONAndClonePreserveOrderedBlocks(t *testing.T) {
	img, ok := ParseImageDataURL(pngDataURL(t))
	if !ok {
		t.Fatal("invalid fixture")
	}
	original := Content{TextBlock("before"), {Image: img}, TextBlock(""), TextBlock("after")}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored Content
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Fatalf("round trip changed blocks: %s", raw)
	}
	cloned := original.Clone()
	*cloned[0].Text = "changed"
	cloned[1].Image.Data[0] ^= 0xff
	if original.Text() != "before\n\nafter" || bytes.Equal(original[1].Image.Data, cloned[1].Image.Data) {
		t.Fatal("clone aliased content")
	}
}

func TestContentReadsLegacyTextAndRejectsInvalidBlocks(t *testing.T) {
	for _, text := range []string{"", "ordinary text", pngDataURL(t)} {
		raw, _ := json.Marshal(text)
		var c Content
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if c.Text() != text || len(c.ImageURLs()) != 0 {
			t.Fatal("legacy text was reinterpreted as an image")
		}
	}
	for _, raw := range []string{`[{}]`, `[{"text":"text","image":{"mime_type":"image/png","data":"AA=="}}]`, `[{"image":{"mime_type":"image/png","data":"AA=="}}]`} {
		var c Content
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			t.Fatalf("accepted invalid blocks: %s", raw)
		}
	}
}
