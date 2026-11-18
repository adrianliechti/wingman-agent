#include "textflag.h"

TEXT audio_AudioQueueNewInput_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_AudioQueueNewInput(SB)
GLOBL	·fnNewInput(SB), RODATA, $8
DATA	·fnNewInput(SB)/8, $audio_AudioQueueNewInput_trampoline<>(SB)

TEXT audio_AudioQueueAllocateBuffer_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_AudioQueueAllocateBuffer(SB)
GLOBL	·fnAllocateBuffer(SB), RODATA, $8
DATA	·fnAllocateBuffer(SB)/8, $audio_AudioQueueAllocateBuffer_trampoline<>(SB)

TEXT audio_AudioQueueEnqueueBuffer_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_AudioQueueEnqueueBuffer(SB)
GLOBL	·fnEnqueueBuffer(SB), RODATA, $8
DATA	·fnEnqueueBuffer(SB)/8, $audio_AudioQueueEnqueueBuffer_trampoline<>(SB)

TEXT audio_AudioQueueStart_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_AudioQueueStart(SB)
GLOBL	·fnStart(SB), RODATA, $8
DATA	·fnStart(SB)/8, $audio_AudioQueueStart_trampoline<>(SB)

TEXT audio_AudioQueueStop_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_AudioQueueStop(SB)
GLOBL	·fnStop(SB), RODATA, $8
DATA	·fnStop(SB)/8, $audio_AudioQueueStop_trampoline<>(SB)

TEXT audio_AudioQueueDispose_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_AudioQueueDispose(SB)
GLOBL	·fnDispose(SB), RODATA, $8
DATA	·fnDispose(SB)/8, $audio_AudioQueueDispose_trampoline<>(SB)

TEXT audio_dispatch_semaphore_create_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_dispatch_semaphore_create(SB)
GLOBL	·fnSemCreate(SB), RODATA, $8
DATA	·fnSemCreate(SB)/8, $audio_dispatch_semaphore_create_trampoline<>(SB)

TEXT audio_dispatch_semaphore_wait_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_dispatch_semaphore_wait(SB)
GLOBL	·fnSemWait(SB), RODATA, $8
DATA	·fnSemWait(SB)/8, $audio_dispatch_semaphore_wait_trampoline<>(SB)

TEXT audio_dispatch_semaphore_signal_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_dispatch_semaphore_signal(SB)
GLOBL	·fnSemSignal(SB), RODATA, $8
DATA	·fnSemSignal(SB)/8, $audio_dispatch_semaphore_signal_trampoline<>(SB)

TEXT audio_dispatch_time_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_dispatch_time(SB)
GLOBL	·fnTime(SB), RODATA, $8
DATA	·fnTime(SB)/8, $audio_dispatch_time_trampoline<>(SB)

TEXT audio_dispatch_release_trampoline<>(SB),NOSPLIT,$0-0
	JMP	audio_dispatch_release(SB)
GLOBL	·fnRelease(SB), RODATA, $8
DATA	·fnRelease(SB)/8, $audio_dispatch_release_trampoline<>(SB)
