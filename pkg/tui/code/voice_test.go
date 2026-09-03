package code

import "testing"

func TestInsertVoiceTextAtCursor(t *testing.T) {
	editor := NewEditor()
	editor.SetText("beforeafter")
	insertVoiceText(editor, 6, " spoken words ")
	if got, want := editor.Text(), "before spoken words after"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	if got, want := editor.cursor, len([]rune("before spoken words ")); got != want {
		t.Fatalf("cursor = %d, want %d", got, want)
	}
}

func TestInsertVoiceTextPreservesExistingWhitespace(t *testing.T) {
	editor := NewEditor()
	editor.SetText("before  after")
	insertVoiceText(editor, 7, "spoken")
	if got, want := editor.Text(), "before spoken after"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}
