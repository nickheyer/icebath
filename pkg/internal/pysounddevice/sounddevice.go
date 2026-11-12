package pysounddevice

import (
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	paUnanticipatedHostError = -9999
	paStreamIsNotStopped     = -9982
	paInputOverflowed        = -9981
	paOutputUnderflowed      = -9980
	paInt16                  = 0x8
	paNoFlag                 = 0
	paContinue               = 0
	paComplete               = 1
	paAbort                  = 2
	paMME                    = 2
	paDirectSound            = 1
	paASIO                   = 3
)

// PortAudioError is sounddevice.PortAudioError with its message, PaErrorCode and host error details
type PortAudioError struct {
	Msg           string
	Code          int
	HasCode       bool
	HostAPIName   string
	HostErrorCode int
	HostErrorText string
	HasHostError  bool
}

// Message, error code and host API error rendered the way sounddevice formats them
func (e *PortAudioError) Error() string {
	msg := e.Msg
	if e.HasCode {
		msg = fmt.Sprintf("%s [PaErrorCode %d]", msg, e.Code)
	}
	if e.HasHostError {
		msg = fmt.Sprintf("%s [%s error %d: %s]", msg, e.HostAPIName, e.HostErrorCode, e.HostErrorText)
	}
	return msg
}

// LibraryError is the OSError sounddevice raises when the PortAudio shared library cannot be found or loaded
type LibraryError struct{ Msg string }

func (e *LibraryError) Error() string { return e.Msg }

// UnsupportedError is the platform gate refusing the audio-io binding on a platform purego cannot load libraries for
type UnsupportedError struct{ Msg string }

func (e *UnsupportedError) Error() string { return e.Msg }

// ValueError is the ValueError sounddevice raises for an unknown device, kind or channel request
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// RuntimeError is the RuntimeError get_stream raises before any play call
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string { return e.Msg }

// CallbackFlags is the PaStreamCallbackFlags bitmask of a stream callback
type CallbackFlags uint32

var callbackFlagNames = []struct {
	bit  CallbackFlags
	name string
}{{0x2, "input overflow"}, {0x1, "input underflow"}, {0x8, "output overflow"}, {0x4, "output underflow"}, {0x10, "priming output"}}

// Comma separated names of the set flags in sounddevice order, empty when none is set
func (f CallbackFlags) String() string {
	var names []string
	for _, n := range callbackFlagNames {
		if f&n.bit != 0 {
			names = append(names, n.name)
		}
	}
	return strings.Join(names, ", ")
}

// CallbackResult is what a callback returns, Complete and Abort stand for raising CallbackStop and CallbackAbort
type CallbackResult int32

const (
	Continue CallbackResult = paContinue
	Complete CallbackResult = paComplete
	Abort    CallbackResult = paAbort
)

// InputCallback receives each captured block of interleaved int16 frames
type InputCallback func(indata []int16, frames int, status CallbackFlags) CallbackResult

// OutputCallback fills each block of interleaved int16 frames PortAudio requests
type OutputCallback func(outdata []int16, frames int, status CallbackFlags) CallbackResult

// Device is one query_devices entry
type Device struct {
	Name                     string
	Index                    int
	HostAPI                  int
	MaxInputChannels         int
	MaxOutputChannels        int
	DefaultLowInputLatency   float64
	DefaultLowOutputLatency  float64
	DefaultHighInputLatency  float64
	DefaultHighOutputLatency float64
	DefaultSamplerate        float64
}

// DeviceList is the query_devices result, truthy when it holds a device
type DeviceList []Device

// HostAPI is one query_hostapis entry
type HostAPI struct {
	Name                string
	Devices             []int
	DefaultInputDevice  int
	DefaultOutputDevice int
}

// StreamParams are the stream constructor keywords, zero values mean the sounddevice defaults
type StreamParams struct {
	Device     any
	Samplerate float64
	Blocksize  int
	Channels   int
}

// Stream is the shared surface of InputStream and OutputStream
type Stream interface {
	Active() (bool, error)
	Stopped() (bool, error)
	Closed() bool
	Start() error
	Stop(ignoreErrors bool) error
	Abort(ignoreErrors bool) error
	Close(ignoreErrors bool) error
	Samplerate() float64
	Blocksize() int
	Channels() int
	Latency() float64
}

// InputStream records int16 frames through a callback or blocking reads
type InputStream interface {
	Stream
	Read(frames int) ([]int16, bool, error)
	ReadAvailable() (int, error)
}

// OutputStream plays int16 frames through blocking writes
type OutputStream interface {
	Stream
	Write(data []int16) (bool, error)
	WriteAvailable() (int, error)
}

// Module is the sounddevice module surface the voice, speaker and wake word code calls
type Module interface {
	QueryDevices() (DeviceList, error)
	QueryDevice(device any, kind string) (Device, error)
	QueryHostAPI(index int) (HostAPI, error)
	Play(data []int16, samplerate float64, blocksize int) error
	Stop()
	GetStream() (Stream, error)
	InputStream(p StreamParams, callback InputCallback) (InputStream, error)
	OutputStream(p StreamParams) (OutputStream, error)
}

type paHostApiInfo struct {
	structVersion       int32
	typeID              int32
	name                *byte
	deviceCount         int32
	defaultInputDevice  int32
	defaultOutputDevice int32
}

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

type paHostErrorInfo struct {
	hostAPIType int32
	errorCode   clong
	errorText   *byte
}

type paStreamParameters struct {
	device                    int32
	channelCount              int32
	sampleFormat              culong
	suggestedLatency          float64
	hostApiSpecificStreamInfo uintptr
}

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
	paIsStreamStopped                 func(stream uintptr) int32
	paIsStreamActive                  func(stream uintptr) int32
	paGetStreamInfo                   func(stream uintptr) *paStreamInfo
	paReadStream                      func(stream uintptr, buffer unsafe.Pointer, frames uint) int32
	paWriteStream                     func(stream uintptr, buffer unsafe.Pointer, frames uint) int32
	paGetStreamReadAvailable          func(stream uintptr) int32
	paGetStreamWriteAvailable         func(stream uintptr) int32
)

var (
	importMu    sync.Mutex
	initialized bool
	streamCB    uintptr
	streams     sync.Map
	nextID      atomic.Uintptr
)

// Loads and binds the PortAudio library without initializing its host APIs
func Load() error {
	importMu.Lock()
	defer importMu.Unlock()
	return openPortAudio()
}

// Import loads PortAudio and initializes it once, failing like importing sounddevice fails
func Import() (Module, error) {
	importMu.Lock()
	defer importMu.Unlock()
	if initialized {
		return module{}, nil
	}
	if err := openPortAudio(); err != nil {
		return nil, err
	}
	restore := ignoreStderr()
	code := paInitialize()
	restore()
	if err := check(code, "Error initializing PortAudio"); err != nil {
		return nil, err
	}
	if streamCB == 0 {
		streamCB = newStreamCallback(dispatchStream)
	}
	initialized = true
	return module{}, nil
}

// Bytes of the NUL terminated C string, empty for a NULL pointer
func cString(p *byte) []byte {
	if p == nil {
		return nil
	}
	var out []byte
	for ptr := unsafe.Pointer(p); *(*byte)(ptr) != 0; ptr = unsafe.Add(ptr, 1) {
		out = append(out, *(*byte)(ptr))
	}
	return out
}

// PortAudioError for a negative code, the host error details attached for an unanticipated host error
func check(code int32, msg string) error {
	if code >= 0 {
		return nil
	}
	text := string(cString(paGetErrorText(code)))
	if msg != "" {
		text = msg + ": " + text
	}
	err := &PortAudioError{Msg: text, Code: int(code), HasCode: true}
	if code == paUnanticipatedHostError {
		if info := paGetLastHostErrorInfo(); info != nil {
			hostIndex := paHostApiTypeIdToHostApiIndex(info.hostAPIType)
			err.HostErrorCode, err.HostErrorText, err.HasHostError = int(info.errorCode), string(cString(info.errorText)), true
			if api, qerr := queryHostAPI(int(hostIndex)); qerr == nil {
				err.HostAPIName = api.Name
			}
		}
	}
	return err
}

type module struct{}

// Every device PortAudio reports, in index order
func (module) QueryDevices() (DeviceList, error) {
	count := paGetDeviceCount()
	if err := check(count, ""); err != nil {
		return nil, err
	}
	list := DeviceList{}
	for i := 0; i < int(count); i++ {
		dev, err := queryDeviceIndex(i)
		if err != nil {
			return nil, err
		}
		list = append(list, dev)
	}
	return list, nil
}

// The device a selector names for one direction, rejecting a device without channels in that direction
func (m module) QueryDevice(device any, kind string) (Device, error) {
	if kind != "input" && kind != "output" && kind != "" {
		return Device{}, &ValueError{Msg: "Invalid kind: " + pyrepr.Repr(kind)}
	}
	index, err := deviceID(device, kind)
	if err != nil {
		return Device{}, err
	}
	dev, err := queryDeviceIndex(index)
	if err != nil {
		return Device{}, err
	}
	if kind == "input" && dev.MaxInputChannels < 1 || kind == "output" && dev.MaxOutputChannels < 1 {
		return Device{}, &ValueError{Msg: fmt.Sprintf("Not an %s device: %s", kind, pyrepr.Repr(dev.Name))}
	}
	return dev, nil
}

func (module) QueryHostAPI(index int) (HostAPI, error) { return queryHostAPI(index) }

// One host API entry with its device indices
func queryHostAPI(index int) (HostAPI, error) {
	info := paGetHostApiInfo(int32(index))
	if info == nil {
		return HostAPI{}, &PortAudioError{Msg: fmt.Sprintf("Error querying host API %d", index)}
	}
	api := HostAPI{Name: string(cString(info.name)), Devices: []int{},
		DefaultInputDevice: int(info.defaultInputDevice), DefaultOutputDevice: int(info.defaultOutputDevice)}
	for i := int32(0); i < info.deviceCount; i++ {
		api.Devices = append(api.Devices, int(paHostApiDeviceIndexToDeviceIndex(int32(index), i)))
	}
	return api, nil
}

// One device entry, its name decoded as UTF-8 or the Windows ANSI code page for MME, DirectSound and ASIO
func queryDeviceIndex(index int) (Device, error) {
	info := paGetDeviceInfo(int32(index))
	if info == nil {
		return Device{}, &PortAudioError{Msg: fmt.Sprintf("Error querying device %d", index)}
	}
	raw := cString(info.name)
	name := string(raw)
	if !utf8.Valid(raw) {
		host := info.hostAPI
		ansi := host == paHostApiTypeIdToHostApiIndex(paDirectSound) || host == paHostApiTypeIdToHostApiIndex(paMME) ||
			host == paHostApiTypeIdToHostApiIndex(paASIO)
		decoded, err := decodeDeviceName(raw, ansi)
		if err != nil {
			return Device{}, err
		}
		name = decoded
	}
	return Device{Name: name, Index: index, HostAPI: int(info.hostAPI), MaxInputChannels: int(info.maxInputChannels),
		MaxOutputChannels: int(info.maxOutputChannels), DefaultLowInputLatency: info.defaultLowInputLatency,
		DefaultLowOutputLatency: info.defaultLowOutputLatency, DefaultHighInputLatency: info.defaultHighInputLatency,
		DefaultHighOutputLatency: info.defaultHighOutputLatency, DefaultSamplerate: info.defaultSampleRate}, nil
}

// Device index for an index, a query string or the default pair, as sounddevice resolves a selector
func deviceID(selector any, kind string) (int, error) {
	if selector == nil {
		in, out := int(paGetDefaultInputDevice()), int(paGetDefaultOutputDevice())
		switch kind {
		case "input":
			return in, nil
		case "output":
			return out, nil
		}
		if in != out {
			return 0, &ValueError{Msg: fmt.Sprintf("Input and output device are different: (%d, %d)", in, out)}
		}
		return in, nil
	}
	if idx, ok := selector.(int); ok {
		return idx, nil
	}
	query, _ := selector.(string)
	devices, err := module{}.QueryDevices()
	if err != nil {
		return 0, err
	}
	return matchDevice(devices, query, kind, func(i int) (string, error) {
		api, err := queryHostAPI(i)
		return api.Name, err
	})
}

// Index of the single device whose name and host API contain every query word in order
func matchDevice(devices DeviceList, query, kind string, hostName func(int) (string, error)) (int, error) {
	type match struct {
		id   int
		full string
	}
	var matches []match
	var exact []int
	lower := strings.ToLower(query)
	words := strings.Fields(lower)
	for _, dev := range devices {
		if kind == "input" && dev.MaxInputChannels <= 0 || kind == "output" && dev.MaxOutputChannels <= 0 {
			continue
		}
		api, err := hostName(dev.HostAPI)
		if err != nil {
			return 0, err
		}
		full := dev.Name + ", " + api
		hay, pos, ok := strings.ToLower(full), 0, true
		for _, w := range words {
			at := strings.Index(hay[pos:], w)
			if at < 0 {
				ok = false
				break
			}
			pos += at + len(w)
		}
		if ok {
			matches = append(matches, match{dev.Index, full})
			if strings.ToLower(dev.Name) == lower {
				exact = append(exact, dev.Index)
			}
		}
	}
	label := kind
	if label == "" {
		label = "input/output"
	}
	if len(matches) == 0 {
		return 0, &ValueError{Msg: "No " + label + " device matching " + pyrepr.Repr(query)}
	}
	if len(matches) > 1 {
		if len(exact) == 1 {
			return exact[0], nil
		}
		lines := make([]string, len(matches))
		for i, m := range matches {
			lines[i] = fmt.Sprintf("[%d] %s", m.id, m.full)
		}
		return 0, &ValueError{Msg: "Multiple " + label + " devices found for " + pyrepr.Repr(query) + ":\n" + strings.Join(lines, "\n")}
	}
	return matches[0].id, nil
}

// PaStreamParameters of one direction with the high default latency and the device default rate
func streamParameters(kind string, p StreamParams) (*paStreamParameters, float64, error) {
	index, err := deviceID(p.Device, kind)
	if err != nil {
		return nil, 0, err
	}
	dev, err := queryDeviceIndex(index)
	if err != nil {
		return nil, 0, err
	}
	channels, latency := p.Channels, dev.DefaultHighOutputLatency
	if kind == "input" {
		latency = dev.DefaultHighInputLatency
		if channels == 0 {
			channels = dev.MaxInputChannels
		}
	} else if channels == 0 {
		channels = dev.MaxOutputChannels
	}
	rate := p.Samplerate
	if rate == 0 {
		rate = dev.DefaultSamplerate
	}
	return &paStreamParameters{device: int32(index), channelCount: int32(channels), sampleFormat: paInt16,
		suggestedLatency: latency}, rate, nil
}

type stream struct {
	ptr        atomic.Uintptr
	id         uintptr
	channels   int
	blocksize  int
	samplerate float64
	latency    float64
	onInput    InputCallback
	onOutput   OutputCallback
}

// Opens a PortAudio stream of one direction, a callback stream when a callback is given
func openStream(class, kind string, p StreamParams, onInput InputCallback, onOutput OutputCallback) (*stream, error) {
	params, rate, err := streamParameters(kind, p)
	if err != nil {
		return nil, err
	}
	s := &stream{id: nextID.Add(1), channels: int(params.channelCount), blocksize: p.Blocksize,
		onInput: onInput, onOutput: onOutput}
	var in, out *paStreamParameters
	if kind == "input" {
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
	if err := check(paOpenStream(&ptr, in, out, rate, uint(p.Blocksize), paNoFlag, callback, s.id), "Error opening "+class); err != nil {
		streams.Delete(s.id)
		return nil, err
	}
	s.ptr.Store(ptr)
	info := paGetStreamInfo(ptr)
	if info == nil {
		return nil, &PortAudioError{Msg: "Could not obtain stream info"}
	}
	s.samplerate = info.sampleRate
	s.latency = info.outputLatency
	if kind == "input" {
		s.latency = info.inputLatency
	}
	return s, nil
}

// Device name bytes decoded with the ANSI code page for Windows MME, DirectSound and ASIO devices, else strict UTF-8
func decodeDeviceName(raw []byte, ansi bool) (string, error) {
	if ansi && runtime.GOOS == "windows" {
		return pycodecs.Decode(raw, pycodecs.LocaleEncoding())
	}
	return pycodecs.Decode(raw, "utf-8")
}

// Routes a PortAudio callback to its stream, aborting the stream when the Go callback panics
func dispatchStream(input, output unsafe.Pointer, frameCount uintptr, _ unsafe.Pointer, statusFlags, userData uintptr) (result uintptr) {
	v, ok := streams.Load(userData)
	if !ok {
		return paAbort
	}
	s := v.(*stream)
	frames, status := int(uint32(frameCount)), CallbackFlags(uint32(statusFlags))
	defer func() {
		if r := recover(); r != nil {
			slog.Error("sounddevice callback failed", "error", r)
			result = paAbort
		}
	}()
	if s.onInput != nil {
		data := make([]int16, frames*s.channels)
		copy(data, unsafe.Slice((*int16)(input), frames*s.channels))
		return uintptr(s.onInput(data, frames, status))
	}
	return uintptr(s.onOutput(unsafe.Slice((*int16)(output), frames*s.channels), frames, status))
}

// Whether the stream is running, an error once it is closed
func (s *stream) Active() (bool, error) {
	code := paIsStreamActive(s.ptr.Load())
	if err := check(code, ""); err != nil {
		return false, err
	}
	return code == 1, nil
}

// Whether the stream is stopped, an error once it is closed
func (s *stream) Stopped() (bool, error) {
	code := paIsStreamStopped(s.ptr.Load())
	if err := check(code, ""); err != nil {
		return false, err
	}
	return code == 1, nil
}

func (s *stream) Closed() bool { return s.ptr.Load() == 0 }

// Starts processing, a stream already running is not an error
func (s *stream) Start() error {
	code := paStartStream(s.ptr.Load())
	if code == paStreamIsNotStopped {
		return nil
	}
	return check(code, "Error starting stream")
}

// Stops after pending buffers play out
func (s *stream) Stop(ignoreErrors bool) error {
	code := paStopStream(s.ptr.Load())
	if ignoreErrors {
		return nil
	}
	return check(code, "Error stopping stream")
}

// Stops at once, dropping pending buffers
func (s *stream) Abort(ignoreErrors bool) error {
	code := paAbortStream(s.ptr.Load())
	if ignoreErrors {
		return nil
	}
	return check(code, "Error aborting stream")
}

// Closes the stream and forgets its pointer and callbacks
func (s *stream) Close(ignoreErrors bool) error {
	code := paCloseStream(s.ptr.Swap(0))
	streams.Delete(s.id)
	if ignoreErrors {
		return nil
	}
	return check(code, "Error closing stream")
}

func (s *stream) Samplerate() float64 { return s.samplerate }
func (s *stream) Blocksize() int      { return s.blocksize }
func (s *stream) Channels() int       { return s.channels }
func (s *stream) Latency() float64    { return s.latency }

// Blocks until frames are captured, reporting an input overflow instead of failing on it
func (s *stream) Read(frames int) ([]int16, bool, error) {
	buf := make([]int16, frames*s.channels+1)
	code := paReadStream(s.ptr.Load(), unsafe.Pointer(&buf[0]), uint(frames))
	if code == paInputOverflowed {
		return buf[:frames*s.channels], true, nil
	}
	if err := check(code, ""); err != nil {
		return nil, false, err
	}
	return buf[:frames*s.channels], false, nil
}

// Blocks until the frames are queued, reporting an output underflow instead of failing on it
func (s *stream) Write(data []int16) (bool, error) {
	frames := len(data) / s.channels
	if frames == 0 {
		return false, nil
	}
	code := paWriteStream(s.ptr.Load(), unsafe.Pointer(&data[0]), uint(frames))
	if code == paOutputUnderflowed {
		return true, nil
	}
	return false, check(code, "")
}

// Frames that can be read without blocking
func (s *stream) ReadAvailable() (int, error) {
	code := paGetStreamReadAvailable(s.ptr.Load())
	return int(code), check(code, "")
}

// Frames that can be written without blocking
func (s *stream) WriteAvailable() (int, error) {
	code := paGetStreamWriteAvailable(s.ptr.Load())
	return int(code), check(code, "")
}

func (module) InputStream(p StreamParams, callback InputCallback) (InputStream, error) {
	s, err := openStream("InputStream", "input", p, callback, nil)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (module) OutputStream(p StreamParams) (OutputStream, error) {
	s, err := openStream("OutputStream", "output", p, nil, nil)
	if err != nil {
		return nil, err
	}
	return s, nil
}

type playContext struct {
	data      []int16
	frame     int
	blocksize int
	status    CallbackFlags
	stream    *stream
}

// Copies the next block into outdata, zero fills the rest and aborts once the buffer is spent
func (c *playContext) callback(outdata []int16, frames int, status CallbackFlags) CallbackResult {
	c.status |= status
	c.blocksize = min(len(c.data)-c.frame, frames)
	copy(outdata[:c.blocksize], c.data[c.frame:c.frame+c.blocksize])
	clear(outdata[c.blocksize:])
	if c.blocksize == 0 {
		return Abort
	}
	c.frame += c.blocksize
	return Continue
}

var (
	lastMu       sync.Mutex
	lastCallback *playContext
)

// Stops the previous playback and starts the buffer on the default output device
func (module) Play(data []int16, samplerate float64, blocksize int) error {
	ctx := &playContext{data: data}
	module{}.Stop()
	s, err := openStream("OutputStream", "output", StreamParams{Samplerate: samplerate, Blocksize: blocksize, Channels: 1},
		nil, ctx.callback)
	if err != nil {
		return err
	}
	ctx.stream = s
	if err := s.Start(); err != nil {
		return err
	}
	lastMu.Lock()
	lastCallback = ctx
	lastMu.Unlock()
	return nil
}

// Stops and closes the stream of the last play call
func (module) Stop() {
	lastMu.Lock()
	ctx := lastCallback
	lastMu.Unlock()
	if ctx != nil {
		_ = ctx.stream.Stop(true)
		_ = ctx.stream.Close(true)
	}
}

// Stream of the last play call
func (module) GetStream() (Stream, error) {
	lastMu.Lock()
	defer lastMu.Unlock()
	if lastCallback == nil {
		return nil, &RuntimeError{Msg: "play()/rec()/playrec() was not called yet"}
	}
	return lastCallback.stream, nil
}
