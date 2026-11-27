package portaudio

import (
	"unsafe"

	"github.com/nickheyer/icebath/internal/speech/native"
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

// C PaHostApiInfo, field for field
type paHostApiInfo struct {
	structVersion       int32
	typeID              int32
	name                *byte
	deviceCount         int32
	defaultInputDevice  int32
	defaultOutputDevice int32
}

// C PaDeviceInfo, field for field
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

// C PaHostErrorInfo, field for field
type paHostErrorInfo struct {
	hostAPIType int32
	errorCode   clong
	errorText   *byte
}

// C PaStreamParameters, field for field
type paStreamParameters struct {
	device                    int32
	channelCount              int32
	sampleFormat              culong
	suggestedLatency          float64
	hostApiSpecificStreamInfo uintptr
}

// C PaStreamInfo, field for field
type paStreamInfo struct {
	structVersion int32
	inputLatency  float64
	outputLatency float64
	sampleRate    float64
}

var (
	paInitialize                  func() int32
	paGetErrorText                func(code int32) *byte
	paGetHostApiInfo              func(index int32) *paHostApiInfo
	paHostApiTypeIdToHostApiIndex func(typeID int32) int32
	paGetLastHostErrorInfo        func() *paHostErrorInfo
	paGetDeviceCount              func() int32
	paGetDefaultInputDevice       func() int32
	paGetDefaultOutputDevice      func() int32
	paGetDeviceInfo               func(device int32) *paDeviceInfo
	paOpenStream                  func(stream *uintptr, in, out *paStreamParameters, rate float64, frames, flags uint, callback, userData uintptr) int32
	paCloseStream                 func(stream uintptr) int32
	paStartStream                 func(stream uintptr) int32
	paStopStream                  func(stream uintptr) int32
	paAbortStream                 func(stream uintptr) int32
	paIsStreamActive              func(stream uintptr) int32
	paGetStreamInfo               func(stream uintptr) *paStreamInfo
	paReadStream                  func(stream uintptr, buffer unsafe.Pointer, frames uint) int32
	paWriteStream                 func(stream uintptr, buffer unsafe.Pointer, frames uint) int32
	paGetStreamReadAvailable      func(stream uintptr) int32
)

// Binds every PortAudio function the package calls, none when the library lacks one
func bind(lib *native.Library) error {
	return lib.BindAll(map[string]any{
		"Pa_Initialize": &paInitialize, "Pa_GetErrorText": &paGetErrorText, "Pa_GetHostApiInfo": &paGetHostApiInfo,
		"Pa_HostApiTypeIdToHostApiIndex": &paHostApiTypeIdToHostApiIndex, "Pa_GetLastHostErrorInfo": &paGetLastHostErrorInfo,
		"Pa_GetDeviceCount": &paGetDeviceCount, "Pa_GetDefaultInputDevice": &paGetDefaultInputDevice,
		"Pa_GetDefaultOutputDevice": &paGetDefaultOutputDevice, "Pa_GetDeviceInfo": &paGetDeviceInfo, "Pa_OpenStream": &paOpenStream,
		"Pa_CloseStream": &paCloseStream, "Pa_StartStream": &paStartStream, "Pa_StopStream": &paStopStream, "Pa_AbortStream": &paAbortStream,
		"Pa_IsStreamActive": &paIsStreamActive, "Pa_GetStreamInfo": &paGetStreamInfo, "Pa_ReadStream": &paReadStream,
		"Pa_WriteStream": &paWriteStream, "Pa_GetStreamReadAvailable": &paGetStreamReadAvailable,
	})
}
