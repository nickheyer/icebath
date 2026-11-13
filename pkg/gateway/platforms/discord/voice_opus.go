package discord

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

// Opus frame geometry and encoder settings of discord.py's opus module
const (
	opusSamplingRate     = 48000
	opusChannels         = 2
	opusFrameLength      = 20
	opusSampleSize       = 4
	opusSamplesPerFrame  = opusSamplingRate / 1000 * opusFrameLength
	opusFrameSize        = opusSamplesPerFrame * opusSampleSize
	opusApplicationAudio = 2049
	opusCtlSetBitrate    = 4002
	opusCtlSetBandwidth  = 4008
	opusCtlSetFEC        = 4012
	opusCtlSetPLP        = 4014
	opusCtlSetSignal     = 4024
	opusBandFull         = 1105
	opusSignalAuto       = -1000
)

// Opus packet that plays as silence, sent around playback the way discord.py does
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// libopus entry points bound once per loaded library
type opusFuncs struct {
	encoderCreate    func(fs, channels, application int32, err unsafe.Pointer) uintptr
	encoderDestroy   func(st uintptr)
	encode           func(st uintptr, pcm unsafe.Pointer, frameSize int32, data unsafe.Pointer, maxBytes int32) int32
	decoderCreate    func(fs, channels int32, err unsafe.Pointer) uintptr
	decoderDestroy   func(st uintptr)
	decode           func(st uintptr, data unsafe.Pointer, length int32, pcm unsafe.Pointer, frameSize, fec int32) int32
	packetNbFrames   func(data unsafe.Pointer, length int32) int32
	packetSamplesPer func(data unsafe.Pointer, fs int32) int32
	strerror         func(code int32) uintptr
	encoderCtl       opusCtlFunc
}

var (
	opusBindMu  sync.Mutex
	opusBound   *opusFuncs
	opusBoundOn *pyctypes.CDLL
)

// Bound functions of the loaded libopus, the not loaded error discord.py raises when none is loaded
func opusAPI() (*opusFuncs, error) {
	opus.mu.Lock()
	lib := opus.lib
	opus.mu.Unlock()
	if lib == nil {
		loadOpusCodec()
		opus.mu.Lock()
		lib = opus.lib
		opus.mu.Unlock()
		if lib == nil {
			return nil, &opusNotLoaded{}
		}
	}
	opusBindMu.Lock()
	defer opusBindMu.Unlock()
	if opusBound != nil && opusBoundOn == lib {
		return opusBound, nil
	}
	f := &opusFuncs{}
	for name, fn := range map[string]any{
		"opus_encoder_create": &f.encoderCreate, "opus_encoder_destroy": &f.encoderDestroy, "opus_encode": &f.encode,
		"opus_decoder_create": &f.decoderCreate, "opus_decoder_destroy": &f.decoderDestroy, "opus_decode": &f.decode,
		"opus_packet_get_nb_frames": &f.packetNbFrames, "opus_packet_get_samples_per_frame": &f.packetSamplesPer,
		"opus_strerror": &f.strerror, "opus_encoder_ctl": &f.encoderCtl,
	} {
		addr, err := lib.Symbol(name)
		if err != nil {
			return nil, err
		}
		pyctypes.RegisterFunc(fn, addr)
	}
	opusBound, opusBoundOn = f, lib
	return f, nil
}

// discord.py OpusNotLoaded raised when an encoder or decoder is built without libopus
type opusNotLoaded struct{}

func (e *opusNotLoaded) Error() string { return "OpusNotLoaded" }

// discord.py OpusError carrying the libopus error string of a negative return code
type opusError struct{ code int32 }

func (e *opusError) Error() string {
	if f, err := opusAPI(); err == nil {
		return pyctypes.GoString(f.strerror(e.code))
	}
	return fmt.Sprintf("opus error %d", e.code)
}

// Opus encoder of a 48 kHz stereo audio stream with discord.py's play defaults
type opusEncoder struct {
	f  *opusFuncs
	st uintptr
}

// Encoder with 128 kbps, FEC at 15 percent expected loss, full band and automatic signal type
func newOpusEncoder() (*opusEncoder, error) {
	f, err := opusAPI()
	if err != nil {
		return nil, err
	}
	var code int32
	st := f.encoderCreate(opusSamplingRate, opusChannels, opusApplicationAudio, unsafe.Pointer(&code))
	if code < 0 {
		return nil, &opusError{code: code}
	}
	e := &opusEncoder{f: f, st: st}
	runtime.SetFinalizer(e, func(e *opusEncoder) { e.f.encoderDestroy(e.st) })
	for _, ctl := range [][2]int32{{opusCtlSetBitrate, 128 * 1024}, {opusCtlSetFEC, 1}, {opusCtlSetPLP, 15}, {opusCtlSetBandwidth, opusBandFull}, {opusCtlSetSignal, opusSignalAuto}} {
		if ret := callOpusCtl(f.encoderCtl, st, ctl[0], ctl[1]); ret < 0 {
			return nil, &opusError{code: ret}
		}
	}
	return e, nil
}

// Opus packet of one PCM frame, at most as many bytes as the PCM input
func (e *opusEncoder) encode(pcm []byte, frameSize int) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, &opusError{code: -1}
	}
	out := make([]byte, len(pcm))
	ret := e.f.encode(e.st, unsafe.Pointer(&pcm[0]), int32(frameSize), unsafe.Pointer(&out[0]), int32(len(out)))
	runtime.KeepAlive(pcm)
	if ret < 0 {
		return nil, &opusError{code: ret}
	}
	return out[:ret], nil
}

// Opus decoder state of one remote SSRC
type voiceOpusDecoder interface {
	decode(data []byte) ([]byte, error)
}

// Opus decoder producing 48 kHz stereo signed 16 bit PCM
type opusDecoder struct {
	f  *opusFuncs
	st uintptr
}

// Builds the decoder of one SSRC, replaced by tests that patch discord.opus.Decoder
var newOpusDecoder = func() (voiceOpusDecoder, error) {
	f, err := opusAPI()
	if err != nil {
		return nil, err
	}
	var code int32
	st := f.decoderCreate(opusSamplingRate, opusChannels, unsafe.Pointer(&code))
	if code < 0 {
		return nil, &opusError{code: code}
	}
	d := &opusDecoder{f: f, st: st}
	runtime.SetFinalizer(d, func(d *opusDecoder) { d.f.decoderDestroy(d.st) })
	return d, nil
}

// PCM of one Opus packet, sized from the packet's frame count and samples per frame
func (d *opusDecoder) decode(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, &opusError{code: -4}
	}
	frames := d.f.packetNbFrames(unsafe.Pointer(&data[0]), int32(len(data)))
	if frames < 0 {
		return nil, &opusError{code: frames}
	}
	samplesPerFrame := d.f.packetSamplesPer(unsafe.Pointer(&data[0]), opusSamplingRate)
	if samplesPerFrame < 0 {
		return nil, &opusError{code: samplesPerFrame}
	}
	frameSize := frames * samplesPerFrame
	pcm := make([]int16, max(int(frameSize)*opusChannels, 1))
	ret := d.f.decode(d.st, unsafe.Pointer(&data[0]), int32(len(data)), unsafe.Pointer(&pcm[0]), frameSize, 0)
	runtime.KeepAlive(data)
	if ret < 0 {
		return nil, &opusError{code: ret}
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&pcm[0])), int(ret)*opusChannels*2), nil
}
