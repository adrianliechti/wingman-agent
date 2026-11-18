package audio

import (
	"context"
	"fmt"
	"io"
	"syscall"
	"time"
	"unsafe"
)

// AudioToolbox is called without cgo, the way golang.org/x/sys/unix calls
// libSystem: dynamic imports, JMP trampolines in capture_darwin.s, and syscall9.

//go:cgo_import_dynamic audio_AudioQueueNewInput AudioQueueNewInput "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic audio_AudioQueueAllocateBuffer AudioQueueAllocateBuffer "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic audio_AudioQueueEnqueueBuffer AudioQueueEnqueueBuffer "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic audio_AudioQueueStart AudioQueueStart "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic audio_AudioQueueStop AudioQueueStop "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic audio_AudioQueueDispose AudioQueueDispose "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic audio_dispatch_semaphore_create dispatch_semaphore_create "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic audio_dispatch_semaphore_wait dispatch_semaphore_wait "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic audio_dispatch_semaphore_signal dispatch_semaphore_signal "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic audio_dispatch_time dispatch_time "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic audio_dispatch_release dispatch_release "/usr/lib/libSystem.B.dylib"

// Trampoline addresses, defined in capture_darwin.s.
var (
	fnNewInput, fnAllocateBuffer, fnEnqueueBuffer, fnStart, fnStop, fnDispose uintptr
	fnSemCreate, fnSemWait, fnSemSignal, fnTime, fnRelease                    uintptr
)

//go:linkname syscall9 syscall.syscall9
func syscall9(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9 uintptr) (r1, r2 uintptr, err syscall.Errno)

func call(fn uintptr, args ...uintptr) uintptr {
	var a [9]uintptr
	copy(a[:], args)
	r, _, _ := syscall9(fn, a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7], a[8])
	return r
}

func osStatus(name string, r uintptr) error {
	if status := int32(r); status != 0 {
		return fmt.Errorf("audio: %s: OSStatus %d", name, status)
	}
	return nil
}

type audioStreamBasicDescription struct {
	SampleRate       float64
	FormatID         uint32
	FormatFlags      uint32
	BytesPerPacket   uint32
	FramesPerPacket  uint32
	BytesPerFrame    uint32
	ChannelsPerFrame uint32
	BitsPerChannel   uint32
	Reserved         uint32
}

// audioQueueBuffer mirrors the leading fields of AudioQueueBuffer.
type audioQueueBuffer struct {
	Capacity uint32
	Data     unsafe.Pointer
	Size     uint32
}

const (
	kAudioFormatLinearPCM           = 0x6C70636D // 'lpcm'
	kLinearPCMFormatFlagIsSignedInt = 1 << 2
	kLinearPCMFormatFlagIsPacked    = 1 << 3
	bufferCount                     = 3
)

// Capture records the default microphone as PCM16LE mono at SampleRate and
// writes it to w in chunks of whole samples until ctx is done. Without
// microphone permission for the terminal, macOS delivers silence, not an error.
func Capture(ctx context.Context, w io.Writer) error {
	format := audioStreamBasicDescription{
		SampleRate:       SampleRate,
		FormatID:         kAudioFormatLinearPCM,
		FormatFlags:      kLinearPCMFormatFlagIsSignedInt | kLinearPCMFormatFlagIsPacked,
		BytesPerPacket:   2,
		FramesPerPacket:  1,
		BytesPerFrame:    2,
		ChannelsPerFrame: 1,
		BitsPerChannel:   16,
	}

	// The queue's callback is dispatch_semaphore_signal itself, so the audio
	// thread never enters Go; filled buffers come back in FIFO order.
	sem := call(fnSemCreate, 0)
	defer call(fnRelease, sem)

	var aq uintptr
	r, _, _ := syscall9(fnNewInput, uintptr(unsafe.Pointer(&format)), fnSemSignal, sem, 0, 0, 0, uintptr(unsafe.Pointer(&aq)), 0, 0)
	if err := osStatus("AudioQueueNewInput", r); err != nil {
		return err
	}
	defer call(fnDispose, aq, 1)

	bufs := make([]*audioQueueBuffer, bufferCount)
	for i := range bufs {
		r, _, _ := syscall9(fnAllocateBuffer, aq, chunkBytes, uintptr(unsafe.Pointer(&bufs[i])), 0, 0, 0, 0, 0, 0)
		if err := osStatus("AudioQueueAllocateBuffer", r); err != nil {
			return err
		}
		if err := osStatus("AudioQueueEnqueueBuffer", call(fnEnqueueBuffer, aq, uintptr(unsafe.Pointer(bufs[i])), 0, 0)); err != nil {
			return err
		}
	}
	if err := osStatus("AudioQueueStart", call(fnStart, aq, 0)); err != nil {
		return err
	}

	next := 0
	wait := func(timeout time.Duration) *audioQueueBuffer {
		if call(fnSemWait, sem, call(fnTime, 0, uintptr(timeout))) != 0 {
			return nil
		}
		buf := bufs[next]
		next = (next + 1) % len(bufs)
		return buf
	}

	for ctx.Err() == nil {
		buf := wait(50 * time.Millisecond)
		if buf == nil {
			continue
		}
		if _, err := w.Write(unsafe.Slice((*byte)(buf.Data), buf.Size)); err != nil {
			call(fnStop, aq, 1)
			return err
		}
		buf.Size = 0
		if err := osStatus("AudioQueueEnqueueBuffer", call(fnEnqueueBuffer, aq, uintptr(unsafe.Pointer(buf)), 0, 0)); err != nil {
			call(fnStop, aq, 1)
			return err
		}
	}

	// A synchronous stop hands back the partially filled buffer.
	call(fnStop, aq, 1)
	for buf := wait(0); buf != nil; buf = wait(0) {
		if _, err := w.Write(unsafe.Slice((*byte)(buf.Data), buf.Size)); err != nil {
			return err
		}
	}
	return nil
}
