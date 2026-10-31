package harness

import (
	"reflect"
	"testing"
)

func TestNewToolPresentationPreservesExecutionPayload(t *testing.T) {
	args := `{"command":"go test ./...","workdir":"pkg"}`
	presentation := NewToolPresentation("shell", "", args, nil)
	if presentation.Title != "Run command" || presentation.Hint != "go test ./..." || presentation.Args != `{"workdir":"pkg"}` {
		t.Fatalf("presentation = %#v", presentation)
	}

	message := toolCallMessage(ToolCall{ID: "call-1", Name: "shell", Args: args})
	call := message.Content[0].ToolCall
	if call.Name != "shell" || call.Args != args || call.Presentation == nil || call.Presentation.Hint != "go test ./..." {
		t.Fatalf("call = %#v", call)
	}
}

func TestNewToolPresentationLocatesEveryBatchEditFile(t *testing.T) {
	args := `{"edits":[{"file_path":"a.go","old_string":"a","new_string":"b"},{"file_path":"b.go","old_string":"c","new_string":"d"},{"file_path":"a.go","old_string":"e","new_string":"f"}]}`
	presentation := NewToolPresentation("edit", "edit", args, nil)
	want := []ToolLocation{{Path: "a.go"}, {Path: "b.go"}}
	if !reflect.DeepEqual(presentation.Locations, want) {
		t.Fatalf("locations = %#v, want %#v", presentation.Locations, want)
	}
}
