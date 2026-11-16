package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winmm                     = windows.NewLazySystemDLL("winmm.dll")
	procWaveInGetNumDevs      = winmm.NewProc("waveInGetNumDevs")
	procWaveInOpen            = winmm.NewProc("waveInOpen")
	procWaveInClose           = winmm.NewProc("waveInClose")
	procWaveInPrepareHeader   = winmm.NewProc("waveInPrepareHeader")
	procWaveInUnprepareHeader = winmm.NewProc("waveInUnprepareHeader")
	procWaveInAddBuffer       = winmm.NewProc("waveInAddBuffer")
	procWaveInStart           = winmm.NewProc("waveInStart")
	procWaveInReset           = winmm.NewProc("waveInReset")
	procWaveInGetErrorTextW   = winmm.NewProc("waveInGetErrorTextW")
)

const (
	waveMapper    = 0xFFFFFFFF
	callbackEvent = 0x00050000
	whdrDone      = 0x00000001
	bufferCount   = 4
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

type waveHdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

const waveHdrSize = unsafe.Sizeof(waveHdr{})

func mmCall(name string, proc *windows.LazyProc, args ...uintptr) error {
	r, _, _ := proc.Call(args...)
	if r == 0 {
		return nil
	}
	text := make([]uint16, 256)
	procWaveInGetErrorTextW.Call(r, uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)))
	return fmt.Errorf("audio: %s: %s", name, windows.UTF16ToString(text))
}

// Capture records the default microphone as PCM16LE mono at SampleRate and
// writes it to w in chunks of whole samples until ctx is done.
func Capture(ctx context.Context, w io.Writer) error {
	if err := procWaveInGetNumDevs.Find(); err != nil {
		return err
	}
	if n, _, _ := procWaveInGetNumDevs.Call(); n == 0 {
		return errors.New("audio: no microphone found")
	}

	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(event)

	format := waveFormatEx{FormatTag: 1, Channels: 1, SamplesPerSec: SampleRate, AvgBytesPerSec: SampleRate * 2, BlockAlign: 2, BitsPerSample: 16}
	var h uintptr
	if err := mmCall("waveInOpen", procWaveInOpen, uintptr(unsafe.Pointer(&h)), waveMapper, uintptr(unsafe.Pointer(&format)), uintptr(event), 0, callbackEvent); err != nil {
		return err
	}

	// The driver fills these asynchronously, so they stay pinned until close.
	hdrs := make([]waveHdr, bufferCount)
	data := make([]byte, bufferCount*chunkBytes)
	var pinner runtime.Pinner
	pinner.Pin(&hdrs[0])
	pinner.Pin(&data[0])

	prepared := 0
	defer func() {
		procWaveInReset.Call(h)
		for i := range prepared {
			procWaveInUnprepareHeader.Call(h, uintptr(unsafe.Pointer(&hdrs[i])), waveHdrSize)
		}
		procWaveInClose.Call(h)
		pinner.Unpin()
	}()

	for i := range hdrs {
		hdrs[i].Data = uintptr(unsafe.Pointer(&data[i*chunkBytes]))
		hdrs[i].BufferLength = chunkBytes
		if err := mmCall("waveInPrepareHeader", procWaveInPrepareHeader, h, uintptr(unsafe.Pointer(&hdrs[i])), waveHdrSize); err != nil {
			return err
		}
		prepared++
		if err := mmCall("waveInAddBuffer", procWaveInAddBuffer, h, uintptr(unsafe.Pointer(&hdrs[i])), waveHdrSize); err != nil {
			return err
		}
	}
	if err := mmCall("waveInStart", procWaveInStart, h); err != nil {
		return err
	}

	next := 0
	for {
		stopped := ctx.Err() != nil
		if stopped {
			// Reset hands back every queued buffer, including the partial one.
			procWaveInReset.Call(h)
		}
		for hdrs[next].Flags&whdrDone != 0 {
			hdr := &hdrs[next]
			if n := int(hdr.BytesRecorded); n > 0 {
				if _, err := w.Write(data[next*chunkBytes : next*chunkBytes+n]); err != nil {
					return err
				}
			}
			hdr.Flags &^= whdrDone
			hdr.BytesRecorded = 0
			if !stopped {
				if err := mmCall("waveInAddBuffer", procWaveInAddBuffer, h, uintptr(unsafe.Pointer(hdr)), waveHdrSize); err != nil {
					return err
				}
			}
			next = (next + 1) % bufferCount
		}
		if stopped {
			return nil
		}
		windows.WaitForSingleObject(event, 50)
	}
}
