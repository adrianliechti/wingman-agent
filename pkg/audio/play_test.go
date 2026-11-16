package audio

import (
	"encoding/binary"
	"testing"
)

func TestRealtimePCM(t *testing.T) {
	pcm := make([]byte, 3200)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(12000))
	}
	out := RealtimePCM(pcm)
	if len(out) != 4800 {
		t.Fatalf("100 ms became %d bytes, want 4800", len(out))
	}
	for i := 0; i < len(out); i += 2 {
		if got := int16(binary.LittleEndian.Uint16(out[i:])); got != 12000 {
			t.Fatalf("constant signal changed to %d", got)
		}
	}
	if RealtimePCM(nil) != nil {
		t.Fatal("empty samples produced audio")
	}
	wav := WAVAtRate(out, RealtimeSampleRate)
	if binary.LittleEndian.Uint32(wav[24:]) != 24000 {
		t.Fatal("wrong playback WAV sample rate")
	}
}
