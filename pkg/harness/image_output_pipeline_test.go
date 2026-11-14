package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fetch"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
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
			if len(message.Content) != 1 || message.Content[0].ToolResult == nil {
				t.Fatal("missing typed result")
			}
			result := message.Content[0].ToolResult
			urls := result.Content.ImageURLs()
			if result.IsError || len(urls) != 1 || result.Content.Text() != "" {
				t.Fatal("image was truncated or converted to text")
			}
			if result.Presentation == nil || result.Presentation.Kind == "" {
				t.Fatal("image presentation lost")
			}
			items := toInput(a.Messages)
			output := items[len(items)-1].OfFunctionCallOutput
			if output == nil {
				t.Fatal("image left its tool output")
			}
			parts := output.Output.OfResponseFunctionCallOutputItemArray
			if len(parts) != 1 || parts[0].OfInputImage == nil || parts[0].OfInputImage.ImageURL.Value != urls[0] {
				t.Fatal("provider lost image")
			}
			assertResultReplay(t, a, call, result.Content)
		})
	}

	t.Run("mixed_text_and_images", func(t *testing.T) {
		imgResult, err := tool.ReadImage(t.Context(), bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		content := append(tool.TextContent("Page title: Dashboard"), imgResult.Content...)
		content = append(content, tool.TextBlock("Details after image"))
		content = append(content, imgResult.Content...)
		mixed := tool.Tool{Name: "mcp_browser_snapshot", Execute: func(context.Context, map[string]any) (tool.Result, error) { return tool.Result{Content: content}, nil }}
		call := ToolCall{ID: "mixed", Name: mixed.Name, Args: "{}"}
		a := &Agent{Config: &Config{Hooks: hook.Hooks{TransformToolResult: []hook.TransformToolResult{truncation.Transform(t.TempDir())}}}}
		if err := a.processToolCalls(t.Context(), []ToolCall{call}, []tool.Tool{mixed}, yieldAll); err != nil {
			t.Fatal(err)
		}
		got := a.Messages[len(a.Messages)-1].Content[0].ToolResult
		if !reflect.DeepEqual(got.Content, content) {
			t.Fatal("block order or payload changed")
		}
		items := toInput(a.Messages)
		output := items[len(items)-1].OfFunctionCallOutput
		if output == nil {
			t.Fatal("images left their tool output")
		}
		parts := output.Output.OfResponseFunctionCallOutputItemArray
		if len(parts) != 4 || parts[0].OfInputText == nil || parts[0].OfInputText.Text != "Page title: Dashboard" || parts[1].OfInputImage == nil ||
			parts[2].OfInputText == nil || parts[2].OfInputText.Text != "Details after image" || parts[3].OfInputImage == nil {
			t.Fatal("provider lost or reordered mixed content")
		}
		assertResultReplay(t, a, call, content)
	})
}

func imageContentFixture(t *testing.T) tool.Content {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	result, err := tool.ReadImage(t.Context(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	return result.Content
}

func assertResultReplay(t *testing.T, a *Agent, call ToolCall, want tool.Content) {
	t.Helper()
	raw, err := json.Marshal(a.StateSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	restored := &Agent{Config: &Config{}}
	if err := restored.Restore(state); err != nil {
		t.Fatal(err)
	}
	got := restored.runSingleToolCall(t.Context(), call, nil)
	if got.IsError || !reflect.DeepEqual(got.Content, want) {
		t.Fatal("replay lost or reordered content")
	}
	// Mutating the returned bytes must not corrupt durable history.
	for _, b := range got.Content {
		if b.Image != nil {
			b.Image.Data[0] ^= 0xff
		}
	}
	again := restored.runSingleToolCall(t.Context(), call, nil)
	if !reflect.DeepEqual(again.Content, want) {
		t.Fatal("replay aliased retained image bytes")
	}
}

func TestTypedToolContentHooksAndLimits(t *testing.T) {
	image := imageContentFixture(t)
	for _, scenario := range []string{"text_data_url", "blocked", "replaced", "transformed", "oversized_text", "invalid_image"} {
		t.Run(scenario, func(t *testing.T) {
			content := append(tool.TextContent("caption"), image.Clone()...)
			cfg := &Config{}
			switch scenario {
			case "text_data_url":
				content = tool.TextContent(image[0].Image.DataURL())
			case "blocked":
				cfg.Hooks.PostToolUse = []hook.PostToolUse{func(context.Context, tool.ToolCall, string) (hook.Outcome, error) {
					return hook.Outcome{Block: true, Reason: "blocked"}, nil
				}}
			case "replaced":
				cfg.Hooks.PostToolUse = []hook.PostToolUse{func(context.Context, tool.ToolCall, string) (hook.Outcome, error) {
					text := "replacement"
					return hook.Outcome{UpdatedResult: &text}, nil
				}}
			case "transformed":
				cfg.Hooks.TransformToolResult = []hook.TransformToolResult{func(_ context.Context, _ tool.ToolCall, text string) (string, error) {
					return strings.ToUpper(text), nil
				}}
			case "oversized_text":
				content[0] = tool.TextBlock(strings.Repeat("x", maxInlineToolResultBytes*2))
				cfg.Hooks.TransformToolResult = []hook.TransformToolResult{truncation.Transform(t.TempDir())}
			case "invalid_image":
				content = tool.Content{{Image: &tool.ImageContent{MIMEType: "image/png", Data: []byte("invalid")}}}
			}
			a := &Agent{Config: cfg}
			result := a.runSingleToolCall(t.Context(), ToolCall{Name: "test"}, []tool.Tool{{Name: "test", Execute: func(context.Context, map[string]any) (tool.Result, error) { return tool.Result{Content: content}, nil }}})
			urls := result.Content.ImageURLs()
			switch scenario {
			case "blocked", "replaced", "text_data_url", "invalid_image":
				if len(urls) != 0 {
					t.Fatal("text replacement leaked image")
				}
			default:
				if len(urls) != 1 {
					t.Fatal("text transform discarded image")
				}
			}
			if scenario == "transformed" && result.Content.Text() != "CAPTION" {
				t.Fatal("transform not applied")
			}
			if scenario == "oversized_text" && len(result.Content.Text()) > maxInlineToolResultBytes {
				t.Fatal("text budget not enforced")
			}
			if scenario == "invalid_image" && !result.IsError {
				t.Fatal("invalid image accepted")
			}
			if scenario == "text_data_url" && result.Content.Text() != image[0].Image.DataURL() {
				t.Fatal("ordinary text changed")
			}
		})
	}
}
