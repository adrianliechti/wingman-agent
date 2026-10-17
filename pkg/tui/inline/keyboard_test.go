package inline

import (
	"strings"
	"testing"
)

func TestModifiedEnterAndDisambiguatedKeys(t *testing.T) {
	cases := []struct {
		sequence string
		want     KeyEvent
	}{
		{"\r", KeyEvent{Key: KeyEnter}},
		{"\x1b\r", KeyEvent{Key: KeyEnter, Alt: true}},
		{"\x1b[13;2u", KeyEvent{Key: KeyEnter, Shift: true}},
		{"\x1b[13;3u", KeyEvent{Key: KeyEnter, Alt: true}},
		{"\x1b[13;4:1u", KeyEvent{Key: KeyEnter, Alt: true, Shift: true}},
		{"\x1b[27;2;13~", KeyEvent{Key: KeyEnter, Shift: true}},
		{"\x1b[27;3;13~", KeyEvent{Key: KeyEnter, Alt: true}},
		{"\x1b[57414;2u", KeyEvent{Key: KeyEnter, Shift: true}},
		{"\x1b[27u", KeyEvent{Key: KeyEsc}},
		{"\x1b[99;5u", KeyEvent{Key: KeyCtrl, Rune: 'c'}},
		{"\x1b[106;5u", KeyEvent{Key: KeyCtrl, Rune: 'j'}},
		{"\x1b[101;3u", KeyEvent{Key: KeyRune, Rune: 'e', Alt: true}},
		{"\x1b[233;1:2u", KeyEvent{Key: KeyRune, Rune: 'é'}},
		{"\x1b[13;66u", KeyEvent{Key: KeyEnter, Shift: true}},
	}
	for _, tc := range cases {
		for split := 0; split <= len(tc.sequence); split++ {
			in, events := testInputReader()
			in.buf = []byte(tc.sequence[:split])
			in.process()
			in.buf = append(in.buf, tc.sequence[split:]...)
			in.process()
			if got := nextEvent(t, events); got != tc.want {
				t.Fatalf("%q split at %d = %#v, want %#v", tc.sequence, split, got, tc.want)
			}
			if len(events) != 0 {
				t.Fatalf("duplicate events for %q", tc.sequence)
			}
		}
	}
}

func TestExtendedKeyboardIgnoresReleasesAndMalformedInput(t *testing.T) {
	for _, sequence := range []string{"13;2:3u", "13;2:0u", "13;0u", "13;999u", "13;9u", "13;2;3u", "55296u", "1114112u", "0u", "13;2:1:3u", "27;2;13;4~"} {
		in, events := testInputReader()
		in.buf = []byte("\x1b[" + sequence)
		in.process()
		if len(events) != 0 {
			t.Errorf("%q produced %#v", sequence, nextEvent(t, events))
		}
	}
}

func TestKeyboardModeRestoredInsideAlternateScreen(t *testing.T) {
	var out strings.Builder
	term := NewTerminal(WithIO(strings.NewReader(""), &out, func() (int, int) { return 80, 24 }))
	term.EnterAlt()
	term.ExitAlt()
	output := out.String()
	enter, push := strings.Index(output, "\x1b[?1049h"), strings.Index(output, "\x1b[>1u")
	pop, leave := strings.Index(output, "\x1b[<u"), strings.Index(output, "\x1b[?1049l")
	if enter < 0 || push <= enter || pop <= push || leave <= pop {
		t.Fatalf("keyboard state was not scoped to the alternate screen: %q", output)
	}
}
