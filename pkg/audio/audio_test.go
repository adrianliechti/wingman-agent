package audio

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func TestWAV(t *testing.T) {
	pcm := []byte{0x01, 0x00, 0xff, 0x7f}
	b := WAV(pcm)

	if len(b) != 44+len(pcm) {
		t.Fatalf("len = %d, want %d", len(b), 44+len(pcm))
	}
	le := binary.LittleEndian
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"riff", string(b[0:4]), "RIFF"},
		{"riff size", le.Uint32(b[4:]), uint32(36 + len(pcm))},
		{"wave fmt", string(b[8:16]), "WAVEfmt "},
		{"fmt size", le.Uint32(b[16:]), uint32(16)},
		{"format", le.Uint16(b[20:]), uint16(1)},
		{"channels", le.Uint16(b[22:]), uint16(1)},
		{"sample rate", le.Uint32(b[24:]), uint32(SampleRate)},
		{"byte rate", le.Uint32(b[28:]), uint32(SampleRate * 2)},
		{"block align", le.Uint16(b[32:]), uint16(2)},
		{"bits", le.Uint16(b[34:]), uint16(16)},
		{"data", string(b[36:40]), "data"},
		{"data size", le.Uint32(b[40:]), uint32(len(pcm))},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if !bytes.Equal(b[44:], pcm) {
		t.Errorf("samples = %x, want %x", b[44:], pcm)
	}
}

func TestLevel(t *testing.T) {
	square := func(amplitude int16, n int) []byte {
		b := make([]byte, 2*n)
		for i := range n {
			v := amplitude
			if i%2 == 1 {
				v = -amplitude
			}
			binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
		}
		return b
	}

	for _, c := range []struct {
		name string
		pcm  []byte
		want float64
	}{
		{"empty", nil, 0},
		{"silence", make([]byte, 320), 0},
		{"full scale", square(32767, 160), 32767.0 / 32768},
		{"half scale", square(16384, 160), 0.5},
		{"odd trailing byte ignored", append(square(16384, 160), 0xff), 0.5},
	} {
		if got := Level(c.pcm); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: Level = %v, want %v", c.name, got, c.want)
		}
	}
}
