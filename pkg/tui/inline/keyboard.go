package inline

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// extendedKey handles CSI-u/Kitty and xterm modifyOtherKeys. Only press and
// repeat events reach the editor; releasing a key must never submit a prompt.
// See https://sw.kovidgoyal.net/kitty/keyboard-protocol/.
func extendedKey(seq string) (KeyEvent, bool) {
	if seq == "" {
		return KeyEvent{}, false
	}
	parts := strings.Split(seq[:len(seq)-1], ";")
	code, modifiers := "", "1"
	switch seq[len(seq)-1] {
	case 'u':
		if len(parts) > 2 {
			return KeyEvent{}, false
		}
		keys := strings.Split(parts[0], ":")
		if len(keys) > 3 {
			return KeyEvent{}, false
		}
		code = keys[0]
		if len(parts) == 2 {
			modifiers = parts[1]
		}
	case '~':
		if len(parts) != 3 || parts[0] != "27" {
			return KeyEvent{}, false
		}
		code, modifiers = parts[2], parts[1]
	default:
		return KeyEvent{}, false
	}
	modifier, eventType, typed := strings.Cut(modifiers, ":")
	if typed && eventType != "1" && eventType != "2" {
		return KeyEvent{}, false
	}
	m, err := strconv.Atoi(modifier)
	if err != nil || m < 1 || m > 256 {
		return KeyEvent{}, false
	}
	m = (m - 1) & 63 // Ignore Caps Lock / Num Lock.
	if m&56 != 0 {   // Do not turn Super/Hyper/Meta chords into plain input.
		return KeyEvent{}, false
	}
	n, err := strconv.Atoi(code)
	if err != nil || n < 0 || n > utf8.MaxRune || !utf8.ValidRune(rune(n)) {
		return KeyEvent{}, false
	}
	k := KeyEvent{Alt: m&2 != 0, Shift: m&1 != 0}
	switch n {
	case 13, 57414: // Enter and keypad Enter.
		k.Key = KeyEnter
	case 27:
		k.Key = KeyEsc
	case 9:
		k.Key = KeyTab
		if k.Shift {
			k.Key = KeyBacktab
		}
	case 127:
		k.Key = KeyBackspace
	default:
		if unicode.IsControl(rune(n)) || n >= 57344 && n <= 63743 {
			return KeyEvent{}, false
		}
		k.Key, k.Rune = KeyRune, rune(n)
		if m&4 != 0 {
			k.Key, k.Rune = KeyCtrl, unicode.ToLower(k.Rune)
			if k.Rune == 'v' {
				// Clipboard presses are handled by pasteKey, which also resolves
				// physical keys and deliberately ignores key-repeat events.
				return KeyEvent{}, false
			}
		}
	}
	return k, true
}
