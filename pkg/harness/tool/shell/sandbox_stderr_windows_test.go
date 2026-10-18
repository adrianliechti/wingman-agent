//go:build windows

package shell

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestPowerShellStartupStderrFilter(t *testing.T) {
	const marker = "__startup_done__"
	diagnostic := powerShellDefaultDrivesDiagnostic
	large := strings.Repeat("x", 10_000)
	for _, tt := range []struct {
		name, input, want string
	}{
		{"startup diagnostic", diagnostic + "\r\n" + marker + "\r\nuser error", "user error"},
		{"ANSI diagnostic", "\x1b[31;1m" + diagnostic + "\x1b[0m\n" + marker + "\nerror", "error"},
		{"BOM diagnostic", "\ufeff" + diagnostic + "\n" + marker + "\n", ""},
		{"no diagnostic", marker + "\nerror without newline", "error without newline"},
		{"unrelated startup error", "access denied\n" + diagnostic + "\n" + marker + "\nerror", "access denied\nerror"},
		{"different provider", strings.Replace(diagnostic, "FileSystem", "Registry", 1) + "\n" + marker + "\n", strings.Replace(diagnostic, "FileSystem", "Registry", 1) + "\n"},
		{"user prints diagnostic", diagnostic + "\n" + marker + "\n" + diagnostic + "\n", diagnostic + "\n"},
		{"marker follows unfinished error", "unfinished" + marker + "\r\nerror", "unfinishederror"},
		{"marker split across reads", strings.Repeat("x", 4090) + marker + "\r\n" + diagnostic, strings.Repeat("x", 4090) + diagnostic},
		{"marker newline split across reads", strings.Repeat("x", 4096-len(marker)) + marker + "\r\n" + diagnostic, strings.Repeat("x", 4096-len(marker)) + diagnostic},
		{"long startup line", large + "\n" + diagnostic + "\n" + marker + "\nerror", large + "\nerror"},
		{"diagnostic within long line", large + diagnostic + "\n" + marker + "\n", large + diagnostic + "\n"},
		{"missing marker", "startup failure\n", "startup failure\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, fragmented := range []bool{false, true} {
				var src io.Reader = strings.NewReader(tt.input)
				if fragmented {
					src = iotest.OneByteReader(src)
				}
				var output bytes.Buffer
				if err := copyPowerShellStartupStderr(&output, src, marker); err != nil {
					t.Fatal(err)
				}
				if output.String() != tt.want {
					t.Fatalf("fragmented=%t: got %q, want %q", fragmented, output.String(), tt.want)
				}
			}
		})
	}
}

func TestPowerShellStartupStderrStreamsAfterMarker(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	output := make(chan string, 4)
	done := make(chan error, 1)
	go func() { done <- copyPowerShellStartupStderr(sandboxStderrChannel(output), reader, "marker") }()
	if _, err := io.WriteString(writer, "marker\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "error without newline"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-output:
		if got != "error without newline" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("stderr buffered after startup")
	}
	writer.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type sandboxStderrChannel chan string

func (w sandboxStderrChannel) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w <- string(p)
	}
	return len(p), nil
}

func TestPowerShellStartupStderrPropagatesWriteFailure(t *testing.T) {
	errWrite := errors.New("stderr write failure")
	if err := copyPowerShellStartupStderr(sandboxStderrErrorWriter{errWrite}, strings.NewReader("other error\n"), "marker"); !errors.Is(err, errWrite) {
		t.Fatalf("got %v, want %v", err, errWrite)
	}
}

type sandboxStderrErrorWriter struct{ err error }

func (w sandboxStderrErrorWriter) Write([]byte) (int, error) { return 0, w.err }
