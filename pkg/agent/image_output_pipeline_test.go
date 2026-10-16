package agent

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fetch"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
)

func TestReadAndFetchImageOutputPipeline(t *testing.T) {
	bitmap := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	random := rand.New(rand.NewPCG(1, 2))
	for i := range bitmap.Pix {
		bitmap.Pix[i] = byte(random.Uint32())
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, bitmap); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= tool.MaxInlineResultBytes {
		t.Fatal("fixture must exceed text output limits")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "image.png"), buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(buf.Bytes()) }))
	defer server.Close()
	for _, tc := range []struct {
		tool tool.Tool
		args map[string]any
	}{
		{fs.ReadTool(root), map[string]any{"file_path": "image.png"}},
		{fetch.Tools(nil)[0], map[string]any{"url": server.URL}},
	} {
		t.Run(tc.tool.Name, func(t *testing.T) {
			args, _ := json.Marshal(tc.args)
			call := ToolCall{ID: "image", Name: tc.tool.Name, Args: string(args)}
			a := &Agent{Config: &Config{Hooks: hook.Hooks{TransformToolResult: []hook.TransformToolResult{truncation.Transform(t.TempDir())}}}}
			if err := a.processToolCalls(t.Context(), []ToolCall{call}, []tool.Tool{tc.tool}, yieldAll); err != nil {
				t.Fatal(err)
			}
			message := a.Messages[len(a.Messages)-1]
			if len(message.Content) != 2 || message.Content[0].ToolResult == nil || message.Content[1].File == nil {
				t.Fatal("tool result did not retain the image attachment")
			}
			result, file := message.Content[0].ToolResult, message.Content[1].File
			if result.IsError || result.Content != imageResultPlaceholder || !tool.IsImageResult(file.Data) {
				t.Fatal("image was truncated or converted to ordinary text")
			}
			if result.Presentation == nil || result.Presentation.Kind == "" {
				t.Fatal("image tool presentation was lost")
			}
			items := toInput(a.Messages)
			imageInput := items[len(items)-1].OfInputMessage
			if imageInput == nil || len(imageInput.Content) != 1 || imageInput.Content[0].OfInputImage == nil || imageInput.Content[0].OfInputImage.ImageURL.Value != file.Data {
				t.Fatal("provider input lost the image attachment")
			}
			// Restore without executors: the retained image must avoid a second
			// file read or network request and keep the original data URL.
			restored := &Agent{Config: &Config{}, Messages: a.Messages}
			if got := restored.runSingleToolCall(t.Context(), call, nil); got.IsError || got.Content != file.Data {
				t.Fatal("replayed tool result lost image data")
			}
		})
	}
}
