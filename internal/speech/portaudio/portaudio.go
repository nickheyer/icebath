// PortAudio, loaded at run time without cgo: the microphones and speakers of this machine, streams of 16 bit samples, one playback of a
// buffer at a time
package portaudio

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/nickheyer/icebath/internal/speech/native"
)

// Way audio flows through a device or a stream
type Direction uint8

const (
	Input Direction = iota + 1
	Output
)

func (d Direction) String() string {
	switch d {
	case Input:
		return "input"
	case Output:
		return "output"
	}
	return "direction " + strconv.Itoa(int(d))
}

// One audio device as PortAudio reports it
type Device struct {
	Name                     string
	Index                    int
	HostAPI                  int
	MaxInputChannels         int
	MaxOutputChannels        int
	DefaultHighInputLatency  time.Duration
	DefaultHighOutputLatency time.Duration
	DefaultSampleRate        float64
}

func (d Device) channels(dir Direction) int {
	if dir == Input {
		return d.MaxInputChannels
	}
	return d.MaxOutputChannels
}

// One host API such as ALSA, Core Audio or WASAPI
type HostAPI struct {
	Name string
}

// This build has no loader for PortAudio
var ErrUnsupported = fmt.Errorf("PortAudio can't be loaded, because %w", native.ErrUnsupported)

// Any call on a stream once Close has begun
var ErrClosed = errors.New("portaudio stream is closed")

// Playback asked before any Play
var ErrNoPlayback = errors.New("no portaudio playback started")

// Audio that does not match the stream's channels
var ErrChannelMismatch = errors.New("audio data does not match the stream's channel count")

var errNoLibrary = errors.New("PortAudio is not installed")

// PortAudio could not be found, opened or bound
type LibraryError struct{ Err error }

func (e *LibraryError) Error() string {
	switch {
	case e.Err == nil:
		return "PortAudio can't be loaded"
	case errors.Is(e.Err, errNoLibrary):
		return e.Err.Error()
	}
	return "PortAudio can't be loaded: " + e.Err.Error()
}

func (e *LibraryError) Unwrap() error { return e.Err }

// A PortAudio call that failed, with the host API's own error when PortAudio reports one
type Error struct {
	Op       string
	Code     int
	Text     string
	HostAPI  string
	HostCode int
	HostText string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op + ": ")
	}
	b.WriteString(e.Text)
	if e.Code != 0 {
		b.WriteString(" (PortAudio error " + strconv.Itoa(e.Code) + ")")
	}
	if e.HostAPI != "" || e.HostCode != 0 || e.HostText != "" {
		fmt.Fprintf(&b, ": %s error %d: %s", cmpOr(e.HostAPI, "host API"), e.HostCode, e.HostText)
	}
	return b.String()
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// A selector that names no device with channels in a direction, or a name that matches several
type DeviceError struct {
	Selector  any
	Direction Direction
	Matches   []string
}

func (e *DeviceError) Error() string {
	if len(e.Matches) > 0 {
		return fmt.Sprintf("%d %s devices match “%s”, so give the whole name of one:\n%s", len(e.Matches), e.Direction, fmt.Sprint(e.Selector),
			strings.Join(e.Matches, "\n"))
	}
	switch sel := e.Selector.(type) {
	case nil:
		return "there is no default " + e.Direction.String() + " device"
	case int:
		return fmt.Sprintf("there is no %s device number %d", e.Direction, sel)
	}
	return fmt.Sprintf("no %s device is named “%s”", e.Direction, fmt.Sprint(e.Selector))
}

// Error of a negative PortAudio code, with the host API's details for an unanticipated host error
func check(code int32, op string) error {
	if code >= 0 {
		return nil
	}
	err := &Error{Op: op, Code: int(code), Text: native.Text(paGetErrorText(code))}
	if code == paUnanticipatedHostError {
		if info := paGetLastHostErrorInfo(); info != nil {
			err.HostCode, err.HostText = int(info.errorCode), native.Text(info.errorText)
			if api, qerr := hostAPI(int(paHostApiTypeIdToHostApiIndex(info.hostAPIType))); qerr == nil {
				err.HostAPI = api.Name
			}
		}
	}
	return err
}

// PortAudio of this process, loaded and initialized
type Audio struct{}

var (
	loadMu      sync.Mutex
	library     *native.Library
	initialized bool
)

// Loads and binds PortAudio without starting its host APIs: whether it is installed
func Load() error {
	loadMu.Lock()
	defer loadMu.Unlock()
	return load()
}

// PortAudio loaded and initialized once per process, what it and the libraries under it print routed into the debug log
func Open() (Audio, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if initialized {
		return Audio{}, nil
	}
	if err := load(); err != nil {
		return Audio{}, err
	}
	var code int32
	quietly(func() { code = paInitialize() })
	if err := check(code, "initialize portaudio"); err != nil {
		return Audio{}, err
	}
	if callback == 0 {
		callback, callbackErr = native.NewCallback(dispatch)
		if callback == 0 && callbackErr == nil {
			callbackErr = native.ErrNoCallback
		}
	}
	initialized = true
	return Audio{}, nil
}

// Binds the first PortAudio that opens and has every function, the platform's usual names and paths first, then the loader's search
func load() error {
	if library != nil {
		return nil
	}
	if !native.Supported {
		return ErrUnsupported
	}
	var first error
	tried := map[string]bool{}
	try := func(candidate string) bool {
		if candidate == "" || tried[candidate] {
			return false
		}
		tried[candidate] = true
		if filepath.Base(candidate) != candidate && !isFile(candidate) {
			return false
		}
		lib, err := native.Open(candidate, false)
		if err == nil {
			err = bind(lib)
		}
		if err != nil {
			if first == nil {
				first = err
			}
			return false
		}
		library = lib
		hookLogs(lib)
		return true
	}
	for _, candidate := range candidates() {
		if try(candidate) {
			return nil
		}
	}
	if try(native.Find("portaudio")) {
		return nil
	}
	if first == nil {
		first = errNoLibrary
	}
	return &LibraryError{Err: first}
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Every device PortAudio reports, in index order
func (Audio) Devices() ([]Device, error) {
	count := paGetDeviceCount()
	if err := check(count, "count devices"); err != nil {
		return nil, err
	}
	devices := make([]Device, 0, count)
	for i := range int(count) {
		dev, ok := deviceAt(i)
		if !ok {
			return nil, &Error{Op: "query device " + strconv.Itoa(i), Text: "no device information"}
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

// Device a selector names in a direction: nil for the default, an index, or a name whose words all appear in order in "name, host API"
func (a Audio) Device(selector any, dir Direction) (Device, error) {
	index, err := resolve(selector, dir)
	if err != nil {
		return Device{}, err
	}
	dev, ok := deviceAt(index)
	if !ok || dev.channels(dir) < 1 {
		return Device{}, &DeviceError{Selector: selector, Direction: dir}
	}
	return dev, nil
}

func (Audio) HostAPI(index int) (HostAPI, error) { return hostAPI(index) }

func hostAPI(index int) (HostAPI, error) {
	var info *paHostApiInfo
	if index >= 0 && index <= math.MaxInt32 {
		info = paGetHostApiInfo(int32(index))
	}
	if info == nil {
		return HostAPI{}, &Error{Op: "query host API " + strconv.Itoa(index), Text: "no such host API"}
	}
	return HostAPI{Name: native.Text(info.name)}, nil
}

// Device at an index, false when there is none; a name that is not UTF-8 decoded the way its host API encodes names
func deviceAt(index int) (Device, bool) {
	if index < 0 || index > math.MaxInt32 {
		return Device{}, false
	}
	info := paGetDeviceInfo(int32(index))
	if info == nil {
		return Device{}, false
	}
	name := native.Text(info.name)
	if !utf8.ValidString(name) {
		name = deviceName(name, info.hostAPI)
	}
	seconds := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	return Device{Name: name, Index: index, HostAPI: int(info.hostAPI), MaxInputChannels: int(info.maxInputChannels),
		MaxOutputChannels: int(info.maxOutputChannels), DefaultHighInputLatency: seconds(info.defaultHighInputLatency),
		DefaultHighOutputLatency: seconds(info.defaultHighOutputLatency), DefaultSampleRate: info.defaultSampleRate}, true
}

// Device index a selector stands for in a direction
func resolve(selector any, dir Direction) (int, error) {
	switch sel := selector.(type) {
	case nil:
		if dir == Input {
			return int(paGetDefaultInputDevice()), nil
		}
		return int(paGetDefaultOutputDevice()), nil
	case int:
		return sel, nil
	case string:
		devices, err := Audio{}.Devices()
		if err != nil {
			return 0, err
		}
		return match(devices, sel, dir)
	}
	return 0, fmt.Errorf("portaudio device selector of type %T, want nil, an int index or a name", selector)
}

// The one device with channels in the direction whose "name, host API" holds every word of the query in order; among several, the one the
// query names exactly
func match(devices []Device, query string, dir Direction) (int, error) {
	words := strings.Fields(strings.ToLower(query))
	names := map[int]string{}
	var ids, exact []int
	var lines []string
	for _, dev := range devices {
		if dev.channels(dir) <= 0 {
			continue
		}
		api, ok := names[dev.HostAPI]
		if !ok {
			host, err := hostAPI(dev.HostAPI)
			if err != nil {
				return 0, err
			}
			api, names[dev.HostAPI] = host.Name, host.Name
		}
		full := dev.Name + ", " + api
		rest, all := strings.ToLower(full), true
		for _, w := range words {
			at := strings.Index(rest, w)
			if at < 0 {
				all = false
				break
			}
			rest = rest[at+len(w):]
		}
		if !all {
			continue
		}
		ids = append(ids, dev.Index)
		lines = append(lines, "["+strconv.Itoa(dev.Index)+"] "+full)
		if strings.EqualFold(dev.Name, query) {
			exact = append(exact, dev.Index)
		}
	}
	switch {
	case len(ids) == 1:
		return ids[0], nil
	case len(exact) == 1:
		return exact[0], nil
	case len(ids) == 0:
		return 0, &DeviceError{Selector: query, Direction: dir}
	}
	return 0, &DeviceError{Selector: query, Direction: dir, Matches: lines}
}

// Settings of a stream to open: the device as Device takes it, a zero rate or channel count taking the device's own, a zero block size
// letting PortAudio choose
type StreamParams struct {
	Device     any
	SampleRate float64
	BlockSize  int
	Channels   int
}

// Status bits PortAudio passes a callback
type CallbackFlags uint32

func (f CallbackFlags) String() string {
	var names []string
	for _, n := range []struct {
		bit  CallbackFlags
		name string
	}{{0x2, "input overflow"}, {0x1, "input underflow"}, {0x8, "output overflow"}, {0x4, "output underflow"}, {0x10, "priming output"}} {
		if f&n.bit != 0 {
			names = append(names, n.name)
		}
	}
	return strings.Join(names, ", ")
}

// What a callback asks of its stream
type CallbackResult int32

const (
	Continue CallbackResult = 0
	Complete CallbackResult = 1
	Abort    CallbackResult = 2
)

// Receives each captured block, the slice the callback's own to keep
type InputCallback func(in []int16, frames int, status CallbackFlags) CallbackResult

// Fills each block PortAudio asks for, valid only until it returns
type outputCallback func(out []int16, frames int, status CallbackFlags) CallbackResult

var (
	callback    uintptr
	callbackErr error
	streams     sync.Map
	nextID      atomic.Uintptr
)

// Capture stream with a callback for each block, or for blocking reads when there is none
func (Audio) OpenInput(p StreamParams, cb InputCallback) (*Stream, error) {
	return open(Input, p, cb, nil)
}

// Playback stream for blocking writes
func (Audio) OpenOutput(p StreamParams) (*Stream, error) {
	return open(Output, p, nil, nil)
}

// Stream of one direction in 16 bit samples, registered under an id PortAudio hands back to the callback, never a Go pointer
func open(dir Direction, p StreamParams, onInput InputCallback, onOutput outputCallback) (*Stream, error) {
	op := "open " + dir.String() + " stream"
	if (onInput != nil || onOutput != nil) && callback == 0 {
		return nil, fmt.Errorf("%s with a callback: %w", op, callbackErr)
	}
	if p.SampleRate < 0 || p.BlockSize < 0 || p.Channels < 0 {
		return nil, fmt.Errorf("%s: negative sample rate, block size or channel count in %+v", op, p)
	}
	index, err := resolve(p.Device, dir)
	if err != nil {
		return nil, err
	}
	dev, ok := deviceAt(index)
	if !ok || dev.channels(dir) < 1 {
		return nil, &DeviceError{Selector: p.Device, Direction: dir}
	}
	channels, latency := cmpInt(p.Channels, dev.channels(dir)), dev.DefaultHighOutputLatency
	if dir == Input {
		latency = dev.DefaultHighInputLatency
	}
	rate := p.SampleRate
	if rate == 0 {
		rate = dev.DefaultSampleRate
	}
	params := &paStreamParameters{device: int32(index), channelCount: int32(channels), sampleFormat: paInt16, suggestedLatency: latency.Seconds()}
	id := nextID.Add(1)
	s := newStream(id, channels, onInput, onOutput)
	var in, out *paStreamParameters
	if dir == Input {
		in = params
	} else {
		out = params
	}
	cb := uintptr(0)
	if onInput != nil || onOutput != nil {
		cb = callback
	}
	streams.Store(id, s)
	var ptr uintptr
	var code int32
	quietly(func() { code = paOpenStream(&ptr, in, out, rate, uint(p.BlockSize), paNoFlag, cb, id) })
	if err := check(code, op); err != nil {
		streams.Delete(id)
		return nil, err
	}
	s.ptr = ptr
	if paGetStreamInfo(ptr) == nil {
		s.Close()
		return nil, &Error{Op: op, Text: "no stream information"}
	}
	return s, nil
}

func cmpInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// The PaStreamCallback every callback stream shares: finds the stream by its id, hands capture blocks over as copies and playback
// blocks in place, aborting when the Go callback panics
func dispatch(input, output unsafe.Pointer, frameCount uintptr, _ unsafe.Pointer, statusFlags, userData uintptr) (result uintptr) {
	v, ok := streams.Load(userData)
	if !ok {
		return uintptr(Abort)
	}
	s := v.(*Stream)
	frames, status := int(uint32(frameCount)), CallbackFlags(uint32(statusFlags))
	n := frames * s.channels
	defer func() {
		if r := recover(); r != nil {
			slog.Error("portaudio stream callback panicked", "error", r)
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

// One buffer playing through its own output stream
type playback struct {
	data   []int16
	frame  int
	stream *Stream
}

var (
	playMu  sync.Mutex
	playing *playback
)

func (p *playback) fill(out []int16, frames int, _ CallbackFlags) CallbackResult {
	n := min(len(p.data)-p.frame, frames, len(out))
	copy(out[:n], p.data[p.frame:p.frame+n])
	clear(out[n:])
	if n == 0 {
		return Abort
	}
	p.frame += n
	return Continue
}

// Stops the last playback and plays a mono buffer on the default output device
func (Audio) Play(data []int16, rate float64, blockSize int) error {
	playMu.Lock()
	defer playMu.Unlock()
	stopPlayback()
	p := &playback{data: data}
	s, err := open(Output, StreamParams{SampleRate: rate, BlockSize: blockSize, Channels: 1}, nil, p.fill)
	if err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		s.Close()
		return err
	}
	p.stream, playing = s, p
	return nil
}

// Stops and closes the stream of the last Play
func (Audio) Stop() {
	playMu.Lock()
	defer playMu.Unlock()
	stopPlayback()
}

// Stream of the last Play, still given once it stopped
func (Audio) Playback() (*Stream, error) {
	playMu.Lock()
	defer playMu.Unlock()
	if playing == nil {
		return nil, ErrNoPlayback
	}
	return playing.stream, nil
}

func stopPlayback() {
	if playing != nil {
		playing.stream.Stop()
		playing.stream.Close()
	}
}
