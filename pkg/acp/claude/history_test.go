package claude

import "testing"

func TestStripMarkerTags(t *testing.T) {

	for _, in := range []string{
		"<command-name>/model</command-name>",
		"<local-command-stdout>out</local-command-stdout>",
		"<local-command-stderr>err</local-command-stderr>",
		"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>",
	} {
		if _, ok := stripMarkerTags(in); ok {
			t.Errorf("stripMarkerTags(%q) should drop the message", in)
		}
	}

	mixed := "<command-name>/model</command-name>" +
		"<local-command-stdout>Set model to opus</local-command-stdout>" +
		"please continue"
	got, ok := stripMarkerTags(mixed)
	if !ok {
		t.Fatalf("expected mixed content to be kept")
	}
	if want := "please continue"; got != want {
		t.Errorf("stripMarkerTags mixed = %q, want %q", got, want)
	}

	if got, ok := stripMarkerTags("hello"); !ok || got != "hello" {
		t.Errorf("plain text = %q %v", got, ok)
	}
}
