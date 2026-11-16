// Package audio records the default microphone and transcribes speech.
package audio

import (
	"encoding/binary"
	"math"
)

// SampleRate is the rate of the PCM16LE mono stream that Capture writes.
const SampleRate = 16000

// chunkBytes is the size of each write from Capture: 100 ms of whole samples.
const chunkBytes = SampleRate * 2 / 10

// WAV wraps PCM16LE mono samples at SampleRate in a WAV container.
func WAV(pcm []byte) []byte {
	b := make([]byte, 44, 44+len(pcm))
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+len(pcm)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], 1) // mono
	binary.LittleEndian.PutUint32(b[24:], SampleRate)
	binary.LittleEndian.PutUint32(b[28:], SampleRate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(pcm)))
	return append(b, pcm...)
}

// Level returns the RMS of PCM16LE samples, from 0 (silence) to 1 (full scale).
func Level(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := range n {
		s := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		sum += s * s
	}
	return math.Sqrt(sum / float64(n))
}
