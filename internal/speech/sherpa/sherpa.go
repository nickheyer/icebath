// The sherpa-onnx speech engine, loaded at run time from its pinned release: offline voices such as Piper's, and keyword spotting
package sherpa

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/speech/native"
)

// Release of sherpa-onnx the binding speaks to
const Version = "1.13.8"

const releaseBase = "https://github.com/k2-fsa/sherpa-onnx/releases/download/v" + Version + "/sherpa-onnx-v" + Version + "-"

// Release archive of the library for each platform, pinned by size and sha256
var archives = map[string]speech.Asset{
	"linux/amd64":   {Name: "linux-x64-shared-lib.tar.bz2", Size: 9816899, SHA256: "3892d184be41027e18165e67f549cd4e4cdd8dcd73ac5579e97afd55e14e30b6"},
	"linux/arm64":   {Name: "linux-aarch64-shared-cpu-lib.tar.bz2", Size: 12595068, SHA256: "fb98b80628a383909bf83626daca2e0d0ffc93bf1f7d222eaaa4282b388f072f"},
	"linux/riscv64": {Name: "linux-riscv64-shared.tar.bz2", Size: 15092445, SHA256: "56c379bdea52a785bd590ac7d4a70fc6806015375a014b957a0627e3bea628e5"},
	"darwin/arm64":  {Name: "osx-arm64-shared-lib.tar.bz2", Size: 8773198, SHA256: "ae77050cdae565496059d96f5ab33d77b397a0864282a4e4a26e3b3b3effb948"},
	"darwin/amd64":  {Name: "osx-x64-shared-lib.tar.bz2", Size: 10014197, SHA256: "0f88371565a06372889c76266e253abd1b415fcc6f98232cabd968933efb0711"},
	"windows/amd64": {Name: "win-x64-shared-MT-Release-lib.tar.bz2", Size: 8032957, SHA256: "b8eedf41bd6d3779218887b48367bb7a3ece5aaa7667f01f69ee823a12b0a9e7"},
	"windows/arm64": {Name: "win-arm64-shared-MT-Release-lib.tar.bz2", Size: 7622849, SHA256: "22cd2b2b5e35c1132abc74a91ee77f683645437382617af6a4bd5818b9c509f4"},
	"android/arm64": {Name: "android-aarch64-termux-shared.tar.bz2", Size: 16451109, SHA256: "ad7bac42adc6295293fe6d36475936afb227cdc757cf293c02a706f6aca4e0a2"},
}

// CUDA 12 builds, published for two platforms
var gpuArchives = map[string]speech.Asset{
	"linux/amd64": {Name: "cuda-12.x-cudnn-9.x-onnxruntime1.28.2-linux-x64-gpu.tar.bz2", Size: 393010348,
		SHA256: "2132af1ae8c84c84f86ac091e4311839287851d2bf5e660332ebce3898a1e0b2"},
	"windows/amd64": {Name: "cuda-12.x-cudnn-9.x-onnxruntime1.28.2-win-x64-cuda.tar.bz2", Size: 595017373,
		SHA256: "066c5b54dbafaa1388001a9c9837ac1374dbba6d6678f193ca06aa0d8e94d8c3"},
}

func archive(gpu bool) (speech.Asset, bool) {
	table := archives
	if gpu {
		table = gpuArchives
	}
	asset, ok := table[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return speech.Asset{}, false
	}
	asset.URL = releaseBase + asset.Name
	asset.Name = "sherpa-onnx-v" + Version + "-" + asset.Name
	return asset, true
}

// Whether the engine publishes a library for this platform
func Supported() bool {
	_, ok := archive(false)
	return ok && native.Supported
}

func device(gpu bool) string {
	if gpu {
		return "GPU"
	}
	return "CPU"
}

// The loaded library, one kind of device per process
type Module struct {
	gpu bool
	lib *native.Library
	api api
}

var loaded struct {
	sync.Mutex
	module *Module
}

// The engine of this process, its library fetched on first use; the GPU build when asked, which a process can't switch to once the CPU
// one runs
func Load(ctx context.Context, s *speech.Speech, gpu bool) (*Module, error) {
	loaded.Lock()
	defer loaded.Unlock()
	if m := loaded.module; m != nil {
		if m.gpu != gpu {
			return nil, fmt.Errorf("The speech engine of this computer already runs on the %s. Restart icebath to move it to the %s.", device(m.gpu), device(gpu))
		}
		return m, nil
	}
	asset, ok := archive(gpu)
	if !ok || !native.Supported {
		if gpu {
			return nil, errors.New("The speech engine of this computer has no GPU build for " + runtime.GOOS + "/" + runtime.GOARCH + ".")
		}
		return nil, errors.New("The speech engine of this computer has no build for " + runtime.GOOS + "/" + runtime.GOARCH + ".")
	}
	tree, err := s.Tree(ctx, asset)
	if err != nil {
		return nil, err
	}
	name := map[string]string{"windows": "sherpa-onnx-c-api.dll", "darwin": "libsherpa-onnx-c-api.dylib"}[runtime.GOOS]
	if name == "" {
		name = "libsherpa-onnx-c-api.so"
	}
	lib, err := native.Open(filepath.Join(tree, strings.TrimSuffix(asset.Name, ".tar.bz2"), "lib", name), false)
	if err != nil {
		return nil, fmt.Errorf("The library of the speech engine did not load: %w", err)
	}
	m := &Module{gpu: gpu, lib: lib}
	if err := lib.BindAll(m.api.table()); err != nil {
		return nil, fmt.Errorf("The library of the speech engine did not load: %w", err)
	}
	loaded.module = m
	return m, nil
}

// The functions of the C API the binding calls
type api struct {
	createOfflineTts      func(config *cOfflineTtsConfig) uintptr
	destroyOfflineTts     func(tts uintptr)
	offlineTtsGenerate    func(tts uintptr, text *byte, config *cGenerationConfig, callback, arg uintptr) uintptr
	destroyGeneratedAudio func(audio uintptr)
	createKeywordSpotter  func(config *cKeywordSpotterConfig) uintptr
	destroyKeywordSpotter func(spotter uintptr)
	createKeywordStream   func(spotter uintptr) uintptr
	isKeywordStreamReady  func(spotter, stream uintptr) int32
	decodeKeywordStream   func(spotter, stream uintptr)
	resetKeywordStream    func(spotter, stream uintptr)
	getKeywordResult      func(spotter, stream uintptr) uintptr
	destroyKeywordResult  func(result uintptr)
	destroyOnlineStream   func(stream uintptr)
	onlineAcceptWaveform  func(stream uintptr, sampleRate int32, samples *float32, n int32)
}

func (a *api) table() map[string]any {
	return map[string]any{
		"SherpaOnnxCreateOfflineTts": &a.createOfflineTts, "SherpaOnnxDestroyOfflineTts": &a.destroyOfflineTts,
		"SherpaOnnxOfflineTtsGenerateWithConfig": &a.offlineTtsGenerate, "SherpaOnnxDestroyOfflineTtsGeneratedAudio": &a.destroyGeneratedAudio,
		"SherpaOnnxCreateKeywordSpotter": &a.createKeywordSpotter, "SherpaOnnxDestroyKeywordSpotter": &a.destroyKeywordSpotter,
		"SherpaOnnxCreateKeywordStream": &a.createKeywordStream, "SherpaOnnxIsKeywordStreamReady": &a.isKeywordStreamReady,
		"SherpaOnnxDecodeKeywordStream": &a.decodeKeywordStream, "SherpaOnnxResetKeywordStream": &a.resetKeywordStream,
		"SherpaOnnxGetKeywordResult": &a.getKeywordResult, "SherpaOnnxDestroyKeywordResult": &a.destroyKeywordResult,
		"SherpaOnnxDestroyOnlineStream": &a.destroyOnlineStream, "SherpaOnnxOnlineStreamAcceptWaveform": &a.onlineAcceptWaveform,
	}
}

func (m *Module) provider() string {
	if m.gpu {
		return "cuda"
	}
	return "cpu"
}

func requireFiles(paths ...string) error {
	for _, path := range paths {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("The speech model file %s is missing.", path)
		}
	}
	return nil
}

// Model files of one VITS voice, such as Piper's, and the scales it speaks with
type VoiceOptions struct {
	Model       string
	Tokens      string
	DataDir     string
	NoiseScale  float32
	NoiseScaleW float32
	LengthScale float32
	Threads     int
}

// A loaded offline voice
type Voice struct {
	api    *api
	mu     sync.Mutex
	handle uintptr
}

// Loads a voice for offline synthesis
func (m *Module) NewVoice(o VoiceOptions) (*Voice, error) {
	if err := requireFiles(o.Model, o.Tokens); err != nil {
		return nil, err
	}
	var held [][]byte
	config := &cOfflineTtsConfig{maxNumSentences: 1, silenceScale: 0.2}
	config.model.numThreads = int32(max(o.Threads, 1))
	config.model.provider = native.Chars(&held, m.provider())
	config.model.vits = cOfflineTtsVitsModelConfig{model: native.Chars(&held, o.Model), tokens: native.Chars(&held, o.Tokens),
		dataDir: native.Chars(&held, o.DataDir), noiseScale: o.NoiseScale, noiseScaleW: o.NoiseScaleW, lengthScale: o.LengthScale}
	handle := m.api.createOfflineTts(config)
	runtime.KeepAlive(held)
	if handle == 0 {
		return nil, fmt.Errorf("The speech engine refused the voice model at %s.", o.Model)
	}
	return &Voice{api: &m.api, handle: handle}, nil
}

// Mono float samples of the text with their sample rate
func (v *Voice) Speak(text string, speaker int, speed float32) ([]float32, int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.handle == 0 {
		return nil, 0, errors.New("The voice is closed.")
	}
	var held [][]byte
	config := &cGenerationConfig{silenceScale: 0.2, speed: speed, sid: int32(speaker)}
	audio := v.api.offlineTtsGenerate(v.handle, native.Chars(&held, text), config, 0, 0)
	runtime.KeepAlive(held)
	if audio == 0 {
		return nil, 0, errors.New("The speech engine made no audio.")
	}
	defer v.api.destroyGeneratedAudio(audio)
	generated := native.At[cGeneratedAudio](audio)
	return native.Floats(generated.samples, generated.n), int(generated.sampleRate), nil
}

func (v *Voice) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.handle != 0 {
		v.api.destroyOfflineTts(v.handle)
		v.handle = 0
	}
}

// Model files of a streaming keyword spotter and how it decides
type SpotterOptions struct {
	Tokens, Encoder, Decoder, Joiner, KeywordsFile string
	Threshold                                      float64
	Threads                                        int
}

// Spots keywords with a streaming transducer and a keyword list
type Spotter struct {
	api    *api
	handle uintptr
}

// Builds a spotter at sherpa's own defaults beyond the options
func (m *Module) NewSpotter(o SpotterOptions) (*Spotter, error) {
	if err := requireFiles(o.Tokens, o.Encoder, o.Decoder, o.Joiner); err != nil {
		return nil, err
	}
	if o.Threads <= 0 {
		return nil, fmt.Errorf("The keyword spotter needs at least one thread, not %d.", o.Threads)
	}
	// the C API puts its own default in place of a zero
	if o.Threshold == 0 {
		return nil, errors.New("A keyword threshold of 0 can't be used, because the speech engine puts its own default in its place.")
	}
	var held [][]byte
	config := &cKeywordSpotterConfig{
		featConfig: cFeatureConfig{sampleRate: 16000, featureDim: 80},
		modelConfig: cOnlineModelConfig{transducerEncoder: native.Chars(&held, o.Encoder), transducerDecoder: native.Chars(&held, o.Decoder),
			transducerJoiner: native.Chars(&held, o.Joiner), tokens: native.Chars(&held, o.Tokens), numThreads: int32(o.Threads),
			provider: native.Chars(&held, "cpu")},
		maxActivePaths: 4, numTrailingBlanks: 1, keywordsScore: 1.0, keywordsThreshold: float32(o.Threshold),
		keywordsFile: native.Chars(&held, o.KeywordsFile),
	}
	handle := m.api.createKeywordSpotter(config)
	runtime.KeepAlive(held)
	if handle == 0 {
		return nil, errors.New("The speech engine refused the settings of the keyword spotter.")
	}
	return &Spotter{api: &m.api, handle: handle}, nil
}

func (k *Spotter) Close() {
	if k.handle != 0 {
		k.api.destroyKeywordSpotter(k.handle)
		k.handle = 0
	}
}

// One decoding stream of a spotter
type Stream struct {
	api    *api
	handle uintptr
}

func (k *Spotter) Stream() *Stream {
	return &Stream{api: k.api, handle: k.api.createKeywordStream(k.handle)}
}

func (s *Stream) Close() {
	if s.handle != 0 {
		s.api.destroyOnlineStream(s.handle)
		s.handle = 0
	}
}

// Feeds samples between -1 and 1 at the rate
func (s *Stream) Accept(rate int, samples []float32) {
	if len(samples) == 0 {
		return
	}
	s.api.onlineAcceptWaveform(s.handle, int32(rate), &samples[0], int32(len(samples)))
	runtime.KeepAlive(samples)
}

func (k *Spotter) Ready(s *Stream) bool { return k.api.isKeywordStreamReady(k.handle, s.handle) != 0 }

func (k *Spotter) Decode(s *Stream) { k.api.decodeKeywordStream(k.handle, s.handle) }

func (k *Spotter) Reset(s *Stream) { k.api.resetKeywordStream(k.handle, s.handle) }

// Keyword the stream detected, empty when none
func (k *Spotter) Keyword(s *Stream) string {
	result := k.api.getKeywordResult(k.handle, s.handle)
	if result == 0 {
		return ""
	}
	defer k.api.destroyKeywordResult(result)
	return strings.TrimSpace(native.GoString(native.At[cKeywordResult](result).keyword))
}
