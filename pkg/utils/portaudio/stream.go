package portaudioutil

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Settings of a stream to open: Device as Audio.Device takes it, and a zero SampleRate or Channels taking the device's own, a zero BlockSize letting PortAudio choose
type StreamParams struct {
	Device     any
	SampleRate float64
	BlockSize  int
	Channels   int
}

// Surface every stream shares
type Stream interface {
	Active() (bool, error)
	Start() error
	Stop() error
	Abort() error
	Close() error
}

// Stream that captures interleaved int16 frames through its callback or through blocking reads
type InputStream interface {
	Stream
	Read(frames int) ([]int16, bool, error)
	ReadAvailable() (int, error)
}

// Stream that plays interleaved int16 frames through blocking writes
type OutputStream interface {
	Stream
	Write(data []int16) (bool, error)
}

// Status bits PortAudio passes a stream callback
type CallbackFlags uint32

var callbackFlagNames = [...]struct {
	bit  CallbackFlags
	name string
}{{0x2, "input overflow"}, {0x1, "input underflow"}, {0x8, "output overflow"}, {0x4, "output underflow"}, {0x10, "priming output"}}

// Names of the set flags joined by commas, empty when none is set
func (f CallbackFlags) String() string {
	var b strings.Builder
	for _, n := range callbackFlagNames {
		if f&n.bit != 0 {
			if b.Len() > 0 {
				b.WriteString(", ")
			}
			b.WriteString(n.name)
		}
	}
	return b.String()
}

// What a stream callback asks of its stream
type CallbackResult int32

const (
	// Keep the stream running
	Continue CallbackResult = 0
	// Finish once the blocks already handed over have played
	Complete CallbackResult = 1
	// Stop at once, dropping what is still queued
	Abort CallbackResult = 2
)

// Callback that receives each captured block of interleaved int16 samples with its frame count and the status flags, the slice being the callback's own to keep
type InputCallback func(indata []int16, frames int, status CallbackFlags) CallbackResult

// Callback that fills each block of interleaved int16 samples PortAudio asks for, valid only until it returns
type outputCallback func(outdata []int16, frames int, status CallbackFlags) CallbackResult

var (
	streamCB    uintptr
	streamCBErr error
	streams     sync.Map
	nextID      atomic.Uintptr
)

// Capture stream with a callback for each block, or for blocking reads when the callback is nil
func (audio) OpenInput(p StreamParams, callback InputCallback) (InputStream, error) {
	s, err := openStream(Input, p, callback, nil)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Playback stream for blocking writes
func (audio) OpenOutput(p StreamParams) (OutputStream, error) {
	s, err := openStream(Output, p, nil, nil)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// PortAudio stream of one direction in 16-bit samples, driven by a callback when one is given; the stream is registered under an id that PortAudio hands back to the callback, never a Go pointer
func openStream(dir Direction, p StreamParams, onInput InputCallback, onOutput outputCallback) (*stream, error) {
	op := "open " + dir.String() + " stream"
	if (onInput != nil || onOutput != nil) && streamCB == 0 {
		return nil, fmt.Errorf("%s with a callback: %w", op, streamCBErr)
	}
	params, rate, err := streamParameters(dir, p)
	if err != nil {
		return nil, err
	}
	s := newStream(nextID.Add(1), int(params.channelCount), onInput, onOutput)
	var in, out *paStreamParameters
	if dir == Input {
		in = params
	} else {
		out = params
	}
	callback := uintptr(0)
	if onInput != nil || onOutput != nil {
		callback = streamCB
	}
	streams.Store(s.id, s)
	var ptr uintptr
	var code int32
	quietly(func() { code = paOpenStream(&ptr, in, out, rate, uint(p.BlockSize), paNoFlag, callback, s.id) })
	if err := check(code, op); err != nil {
		streams.Delete(s.id)
		return nil, err
	}
	s.ptr = ptr
	if paGetStreamInfo(ptr) == nil {
		_ = s.Close()
		return nil, &Error{Op: op, Text: "no stream information"}
	}
	return s, nil
}

// PaStreamParameters for a direction at the device's high default latency, and the rate to open at
func streamParameters(dir Direction, p StreamParams) (*paStreamParameters, float64, error) {
	if p.SampleRate < 0 || p.BlockSize < 0 || p.Channels < 0 {
		return nil, 0, fmt.Errorf("open %s stream: negative sample rate, block size or channel count in %+v", dir, p)
	}
	index, err := resolveDevice(p.Device, dir)
	if err != nil {
		return nil, 0, err
	}
	dev, ok := deviceAt(index)
	if !ok || dev.channels(dir) < 1 {
		return nil, 0, &DeviceError{Selector: p.Device, Direction: dir}
	}
	channels, latency := p.Channels, dev.DefaultHighOutputLatency
	if dir == Input {
		latency = dev.DefaultHighInputLatency
	}
	if channels == 0 {
		channels = dev.channels(dir)
	}
	rate := p.SampleRate
	if rate == 0 {
		rate = dev.DefaultSampleRate
	}
	return &paStreamParameters{device: int32(index), channelCount: int32(channels), sampleFormat: paInt16,
		suggestedLatency: latency.Seconds()}, rate, nil
}

// PaStreamCallback shared by every callback stream: finds the stream by the id PortAudio passes back, hands capture blocks over as copies and playback blocks in place, and aborts the stream when the Go callback panics
func dispatch(input, output unsafe.Pointer, frameCount uintptr, _ unsafe.Pointer, statusFlags, userData uintptr) (result uintptr) {
	v, ok := streams.Load(userData)
	if !ok {
		return uintptr(Abort)
	}
	s := v.(*stream)
	frames, status := int(uint32(frameCount)), CallbackFlags(uint32(statusFlags))
	n := frames * s.channels
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Error("portaudio stream callback panicked", "error", r)
			result = uintptr(Abort)
		}
	}()
	if s.onInput != nil {
		data := make([]int16, n)
		if input != nil {
			copy(data, unsafe.Slice((*int16)(input), n))
		}
		return uintptr(s.onInput(data, frames, status))
	}
	if s.onOutput == nil || output == nil {
		return uintptr(Continue)
	}
	return uintptr(s.onOutput(unsafe.Slice((*int16)(output), n), frames, status))
}
