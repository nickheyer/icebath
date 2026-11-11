package hostinfo

import "sync/atomic"

var containerPin atomic.Pointer[bool]

// Answers every container read with inside until restore runs, simulating another host
func PinContainer(inside bool) (restore func()) {
	prev := containerPin.Swap(&inside)
	return func() { containerPin.Store(prev) }
}
