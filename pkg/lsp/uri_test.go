package lsp_test

import (
	"runtime"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func TestFileURI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-specific paths")
	}

	tests := []struct {
		path string
		want string
	}{
		{"/home/user/file.go", "file:///home/user/file.go"},
		{"/tmp/test.txt", "file:///tmp/test.txt"},
		{"/path/with spaces/file.go", "file:///path/with%20spaces/file.go"},
	}

	for _, tt := range tests {
		got := FileURI(tt.path)
		if got != tt.want {
			t.Errorf("FileURI(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestFileURI_RoundtripForEscapedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-specific paths")
	}

	uri := FileURI("/tmp/test file.go")
	if uri != "file:///tmp/test%20file.go" {
		t.Errorf("FileURI escaped path = %q", uri)
	}
}
