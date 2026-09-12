//go:build !windows

package theme

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func TestParseBackgroundColor(t *testing.T) {
	for _, tt := range []struct {
		reply, hex string
	}{
		{"\x1b]11;rgb:ffff/ffff/ffff\a", "#ffffff"},
		{"\x1b]11;rgb:1616/1818/2121\x1b\\", "#161821"},
		{"\x1b]11;rgb:AA/bb/CC\a", "#aabbcc"},
		{"\x1b]11;rgb:a/b/c\a", "#aabbcc"},
		{"\x1b]11;rgb:aaa/bbb/ccc\a", "#aabbcc"},
		{"\x1b]10;rgb:0000/0000/0000\a\x1b]11;rgb:ff/ff/ff\a", "#ffffff"},
		{"\x1b]11;rgb:ffff/ffff/ffff", ""},
		{"\x1b]11;rgb:ffff/ffff/ffff\x1b", ""},
		{"\x1b]11;rgb:ffff/ffff\a", ""},
		{"\x1b]11;rgb:ffff/ffff/nope\a", ""},
		{"\x1b]11;rgb:ffff/ffff/ffff/ffff\a", ""},
		{"\x1b]11;rgb:fffff/ffff/ffff\a", ""},
		{"\x1b]10;rgb:ffff/ffff/ffff\a", ""},
	} {
		color, ok := parseBackgroundColor(tt.reply)
		if ok != (tt.hex != "") || ok && color != ansi.Hex(tt.hex) {
			t.Errorf("reply %q: got %06x, %t; want %s", tt.reply, color.Hex(), ok, tt.hex)
		}
	}
}

func TestTerminalBackgroundReadsFragmentedReply(t *testing.T) {
	for _, light := range []bool{false, true} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		component := "0000"
		if light {
			component = "ffff"
		}
		written := make(chan error, 1)
		go func() {
			for _, fragment := range []string{"\x1b]11;rgb:", component + "/", component + "/", component, "\x1b", "\\"} {
				if _, err := io.WriteString(w, fragment); err != nil {
					written <- err
					return
				}
				time.Sleep(time.Millisecond)
			}
			written <- nil
		}()
		got, known := readTerminalBackground(int(r.Fd()), time.Second)
		if !known || got != light {
			t.Errorf("light=%t, known=%t; want light=%t", got, known, light)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
	}
}

func TestTerminalBackgroundTimeoutLeavesNoReader(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	started := time.Now()
	if _, known := readTerminalBackground(int(r.Fd()), 20*time.Millisecond); known {
		t.Fatal("silent terminal reported a background")
	}
	if time.Since(started) > time.Second {
		t.Fatal("probe did not respect its timeout")
	}
	if _, err := io.WriteString(w, "input"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	remaining, err := io.ReadAll(r)
	if err != nil || string(remaining) != "input" {
		t.Fatalf("probe consumed later input: %q, %v", remaining, err)
	}
}
