package server

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestToolImagesSurviveLiveAndRestoredTranscripts(t *testing.T) {
	for _, name := range []string{"read", "fetch", "view_image"} {
		t.Run(name, func(t *testing.T) {
			_, backend, model := newProtocolServer(t)
			controller := backend.session("saved")
			controller.load()
			const image = "data:image/png;base64,AA=="
			messages := []harness.Message{
				{Role: harness.RoleAssistant, Content: []harness.Content{{ToolCall: &harness.ToolCall{ID: "image", Name: name, Args: `{"file_path":"shot.png"}`}}}},
				{Role: harness.RoleAssistant, Content: []harness.Content{
					{ToolResult: &harness.ToolResult{ID: "image", Name: name, Content: tool.TextContent("[image attached below]")}},
					{File: &harness.File{Data: image}},
					{File: &harness.File{Data: "hidden"}, Hidden: true},
				}},
			}
			for _, message := range messages {
				backend.handleTurnEvent(code.TurnEvent{SessionID: "saved", Message: &message})
			}
			check := func() {
				t.Helper()
				controller.mu.Lock()
				defer controller.mu.Unlock()
				entries := controller.entries
				if len(entries) != 2 || entries[0].Type != "tool" || entries[0].ToolResult == nil ||
					entries[1].Type != "assistant" || len(entries[1].Images) != 1 || entries[1].Images[0] != image {
					t.Fatalf("transcript = %+v", entries)
				}
			}
			check()
			controller.apply(Frame{Type: EvtStreamReset})
			check() // completed tool images survive provider retries
			model.mu.Lock()
			model.messages = messages
			model.mu.Unlock()
			controller.replaceHistory()
			check()
		})
	}
}

func TestTypedToolImagesSurviveLiveAndRestoredTranscripts(t *testing.T) {
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	result, err := tool.ReadImage(t.Context(), &pngBytes)
	if err != nil {
		t.Fatal(err)
	}
	url := result.Content[0].Image.DataURL()
	_, backend, model := newProtocolServer(t)
	controller := backend.session("saved")
	controller.load()
	content := append(tool.TextContent("caption"), result.Content...)
	messages := []harness.Message{{Role: harness.RoleAssistant, Content: []harness.Content{{ToolResult: &harness.ToolResult{ID: "image", Name: "read", Content: content}}}}}
	for _, message := range messages {
		backend.handleTurnEvent(code.TurnEvent{SessionID: "saved", Message: &message})
	}
	check := func() {
		t.Helper()
		controller.mu.Lock()
		defer controller.mu.Unlock()
		entries := controller.entries
		if len(entries) != 2 || entries[0].ToolResult == nil || *entries[0].ToolResult != "caption" || len(entries[1].Images) != 1 || entries[1].Images[0] != url {
			t.Fatalf("typed transcript=%+v", entries)
		}
	}
	check()
	model.mu.Lock()
	model.messages = messages
	model.mu.Unlock()
	controller.replaceHistory()
	check()
}

func TestAttachedWorkspaceImageReachesTheModel(t *testing.T) {
	dir := t.TempDir()
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), pngBytes.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"shot.png", filepath.Join(dir, "shot.png")} {
		got := readAttachedWorkspaceImage(path, dir)
		if !tool.IsImageResult(got) {
			t.Fatalf("attached image %s dropped: %q", path, got)
		}
	}
	if got := readAttachedWorkspaceImage("notes.txt", dir); got != "" {
		t.Fatalf("text file attached as image: %q", got)
	}
}
