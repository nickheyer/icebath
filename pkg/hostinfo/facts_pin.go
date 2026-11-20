package hostinfo

import "sync/atomic"

var (
	gpuClassPin           atomic.Pointer[string]
	interactiveSessionPin atomic.Pointer[bool]
)

// Answers every GPU class read with class until restore runs, simulating another host
func PinGPUClass(class string) (restore func()) {
	prev := gpuClassPin.Swap(&class)
	return func() { gpuClassPin.Store(prev) }
}

// Answers every interactive session read with active until restore runs, simulating another session
func PinInteractiveSession(active bool) (restore func()) {
	prev := interactiveSessionPin.Swap(&active)
	return func() { interactiveSessionPin.Store(prev) }
}
