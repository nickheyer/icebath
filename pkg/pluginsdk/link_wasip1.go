//go:build wasip1

package pluginsdk

import (
	"context"
	"errors"
	"sync"
	"unsafe"
)

//go:wasmimport icebath host_call
func hostCall(frame unsafe.Pointer, size uint32) uint64

//go:wasmimport icebath host_read
func hostRead(token uint32, buffer unsafe.Pointer)

//go:wasmimport icebath cancelled
func hostCancelled(id uint64) uint32

// Frames as calls between the WebAssembly module and its host
type wasmLink struct{}

var guest = newPeer(wasmLink{})

// Buffers the host fills or reads, held so the collector keeps them in place
var buffers = map[uint32][]byte{}

func pin(buf []byte) uint32 {
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buf))))
	buffers[ptr] = buf
	return ptr
}

//go:wasmexport icebath_alloc
func guestAlloc(size uint32) uint32 {
	if size == 0 {
		size = 1
	}
	return pin(make([]byte, size))
}

//go:wasmexport icebath_free
func guestFree(ptr uint32) { delete(buffers, ptr) }

//go:wasmexport icebath_call
func guestCall(ptr, size uint32) uint64 {
	frame := buffers[ptr]
	delete(buffers, ptr)
	if uint32(len(frame)) < size {
		return 0
	}
	reply := guest.serve(frame[:size])
	if len(reply) == 0 {
		return 0
	}
	return uint64(pin(reply))<<32 | uint64(len(reply))
}

func (wasmLink) form() string { return "wasm" }

func (wasmLink) roundTrip(_ context.Context, _ int64, frame []byte) ([]byte, error) {
	packed := hostCall(unsafe.Pointer(unsafe.SliceData(frame)), uint32(len(frame)))
	token, size := uint32(packed>>32), uint32(packed)
	if size == 0 {
		return nil, errors.New("icebath returned no reply")
	}
	reply := make([]byte, size)
	hostRead(token, unsafe.Pointer(unsafe.SliceData(reply)))
	return reply, nil
}

func (wasmLink) serveContext(id any) (context.Context, context.CancelFunc) {
	n, _ := integer(id)
	c := &polledContext{Context: context.Background(), id: uint64(n), done: make(chan struct{})}
	return c, c.finish
}

// Context of one host request, cancelled when the host reports its caller gone
type polledContext struct {
	context.Context
	id   uint64
	mu   sync.Mutex
	done chan struct{}
	err  error
}

func (c *polledContext) poll() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil && hostCancelled(c.id) != 0 {
		c.err = context.Canceled
		close(c.done)
	}
	return c.err
}

func (c *polledContext) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = context.Canceled
		close(c.done)
	}
}

func (c *polledContext) Done() <-chan struct{} {
	c.poll()
	return c.done
}

func (c *polledContext) Err() error { return c.poll() }

// Has nothing to do in a WebAssembly module, where icebath calls the exported functions
func Main() {}
