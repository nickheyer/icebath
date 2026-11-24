package portaudioutil

import (
	"unsafe"

	dlutil "github.com/nickheyer/icebath/pkg/utils/dl"
)

// PortAudio constants the binding passes or compares against
const (
	paUnanticipatedHostError = -9999
	paStreamIsStopped        = -9983
	paStreamIsNotStopped     = -9982
	paInputOverflowed        = -9981
	paOutputUnderflowed      = -9980
	paInt16                  = 0x8
	paNoFlag                 = 0
	paDirectSound            = 1
	paMME                    = 2
	paASIO                   = 3
)

// Memory layout of the C PaHostApiInfo, field for field
type paHostApiInfo struct {
	structVersion       int32
	typeID              int32
	name                *byte
	deviceCount         int32
	defaultInputDevice  int32
	defaultOutputDevice int32
}

// Memory layout of the C PaDeviceInfo, field for field
type paDeviceInfo struct {
	structVersion            int32
	name                     *byte
	hostAPI                  int32
	maxInputChannels         int32
	maxOutputChannels        int32
	defaultLowInputLatency   float64
	defaultLowOutputLatency  float64
	defaultHighInputLatency  float64
	defaultHighOutputLatency float64
	defaultSampleRate        float64
}

// Memory layout of the C PaHostErrorInfo, field for field
type paHostErrorInfo struct {
	hostAPIType int32
	errorCode   clong
	errorText   *byte
}

// Memory layout of the C PaStreamParameters, field for field
type paStreamParameters struct {
	device                    int32
	channelCount              int32
	sampleFormat              culong
	suggestedLatency          float64
	hostApiSpecificStreamInfo uintptr
}

// Memory layout of the C PaStreamInfo, field for field
type paStreamInfo struct {
	structVersion int32
	inputLatency  float64
	outputLatency float64
	sampleRate    float64
}

var (
	paInitialize                      func() int32
	paGetErrorText                    func(code int32) *byte
	paGetHostApiInfo                  func(index int32) *paHostApiInfo
	paHostApiTypeIdToHostApiIndex     func(typeID int32) int32
	paHostApiDeviceIndexToDeviceIndex func(hostAPI, index int32) int32
	paGetLastHostErrorInfo            func() *paHostErrorInfo
	paGetDeviceCount                  func() int32
	paGetDefaultInputDevice           func() int32
	paGetDefaultOutputDevice          func() int32
	paGetDeviceInfo                   func(device int32) *paDeviceInfo
	paOpenStream                      func(stream *uintptr, in, out *paStreamParameters, rate float64, frames, flags uint, callback, userData uintptr) int32
	paCloseStream                     func(stream uintptr) int32
	paStartStream                     func(stream uintptr) int32
	paStopStream                      func(stream uintptr) int32
	paAbortStream                     func(stream uintptr) int32
	paIsStreamActive                  func(stream uintptr) int32
	paGetStreamInfo                   func(stream uintptr) *paStreamInfo
	paReadStream                      func(stream uintptr, buffer unsafe.Pointer, frames uint) int32
	paWriteStream                     func(stream uintptr, buffer unsafe.Pointer, frames uint) int32
	paGetStreamReadAvailable          func(stream uintptr) int32
)

// Binds every PortAudio function the package calls, binding none when the library lacks one
func bind(lib *dlutil.Library) error {
	table := [...]struct {
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
		{&paIsStreamActive, "Pa_IsStreamActive"}, {&paGetStreamInfo, "Pa_GetStreamInfo"},
		{&paReadStream, "Pa_ReadStream"}, {&paWriteStream, "Pa_WriteStream"},
		{&paGetStreamReadAvailable, "Pa_GetStreamReadAvailable"},
	}
	var addrs [len(table)]uintptr
	for i, entry := range table {
		addr, err := lib.Symbol(entry.name)
		if err != nil {
			return err
		}
		addrs[i] = addr
	}
	for i, entry := range table {
		dlutil.RegisterFunc(entry.fptr, addrs[i])
	}
	return nil
}

// Copy of a NUL terminated C string PortAudio owns, empty for a null pointer
func cstr(p *byte) string { return dlutil.GoString(uintptr(unsafe.Pointer(p))) }
