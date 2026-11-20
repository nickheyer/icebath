//go:build (darwin || freebsd || linux || netbsd || windows) && !386 && !arm && !faketime

package pysounddevice

import (
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

var libHandle uintptr

// Binds every PortAudio entry point of the loaded library, an OSError style error when one is missing
func bindLibrary(handle uintptr, lookup func(string) (uintptr, error)) error {
	table := []struct {
		fptr any
		name string
	}{
		{&paInitialize, "Pa_Initialize"}, {&paGetErrorText, "Pa_GetErrorText"}, {&paGetHostApiInfo, "Pa_GetHostApiInfo"},
		{&paHostApiTypeIdToHostApiIndex, "Pa_HostApiTypeIdToHostApiIndex"},
		{&paHostApiDeviceIndexToDeviceIndex, "Pa_HostApiDeviceIndexToDeviceIndex"},
		{&paGetLastHostErrorInfo, "Pa_GetLastHostErrorInfo"}, {&paGetDeviceCount, "Pa_GetDeviceCount"},
		{&paGetDefaultInputDevice, "Pa_GetDefaultInputDevice"}, {&paGetDefaultOutputDevice, "Pa_GetDefaultOutputDevice"},
		{&paGetDeviceInfo, "Pa_GetDeviceInfo"}, {&paOpenStream, "Pa_OpenStream"}, {&paCloseStream, "Pa_CloseStream"},
		{&paStartStream, "Pa_StartStream"}, {&paStopStream, "Pa_StopStream"}, {&paAbortStream, "Pa_AbortStream"},
		{&paIsStreamStopped, "Pa_IsStreamStopped"}, {&paIsStreamActive, "Pa_IsStreamActive"},
		{&paGetStreamInfo, "Pa_GetStreamInfo"}, {&paReadStream, "Pa_ReadStream"}, {&paWriteStream, "Pa_WriteStream"},
		{&paGetStreamReadAvailable, "Pa_GetStreamReadAvailable"}, {&paGetStreamWriteAvailable, "Pa_GetStreamWriteAvailable"},
	}
	addrs := make([]uintptr, len(table))
	for i, entry := range table {
		addr, err := lookup(entry.name)
		if err != nil || addr == 0 {
			return &LibraryError{Msg: fmt.Sprintf("PortAudio library lacks %s: %v", entry.name, err)}
		}
		addrs[i] = addr
	}
	for i, entry := range table {
		purego.RegisterFunc(entry.fptr, addrs[i])
	}
	libHandle = handle
	return nil
}

// C function pointer of the PaStreamCallback trampoline
func newStreamCallback(fn func(input, output unsafe.Pointer, frameCount uintptr, timeInfo unsafe.Pointer, statusFlags, userData uintptr) uintptr) uintptr {
	return purego.NewCallback(fn)
}
