//go:build windows

package shell

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

const powerShellDefaultDrivesDiagnostic = "Attempting to perform the InitializeDefaultDrives operation on the 'FileSystem' provider failed."

// PowerShell's optional mapped-drive display-path lookup is denied inside
// AppContainer even when the drive itself works. Hide only that startup line;
// the marker is emitted at the beginning of -Command, before user code. Other
// startup errors and all subsequent stderr pass through without modification.
func copyPowerShellStartupStderr(dst io.Writer, src io.Reader, marker string) error {
	if marker == "" {
		_, err := io.Copy(dst, src)
		return err
	}
	reader := bufio.NewReader(src)
	partial := false
	var tail []byte
	for {
		line, err := reader.ReadSlice('\n')
		if len(tail) > 0 {
			line = append(tail, line...)
			tail = nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// Keep enough of a long line to recognize a marker split across
			// reads, including its optional CRLF, without buffering the line.
			keep := min(len(marker)+2, len(line))
			if err := writeSandboxStderr(dst, line[:len(line)-keep]); err != nil {
				return err
			}
			tail = bytes.Clone(line[len(line)-keep:])
			partial = true
			continue
		}
		body := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte{'\n'}), []byte{'\r'})
		if bytes.HasSuffix(body, []byte(marker)) {
			// A previous startup error may not end in a newline. Preserve it
			// while removing the marker and its own newline.
			if err := writeSandboxStderr(dst, body[:len(body)-len(marker)]); err != nil {
				return err
			}
			_, err := io.Copy(dst, reader)
			return err
		}
		text := strings.TrimPrefix(ansi.Strip(string(body)), "\ufeff")
		if partial || text != powerShellDefaultDrivesDiagnostic {
			if err := writeSandboxStderr(dst, line); err != nil {
				return err
			}
		}
		partial = false
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func writeSandboxStderr(dst io.Writer, data []byte) error {
	n, err := dst.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
