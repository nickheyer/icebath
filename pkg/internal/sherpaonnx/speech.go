package sherpaonnx

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/fetch"
)

const releaseBase = "https://github.com/k2-fsa/sherpa-onnx/releases/download/v" + Version + "/sherpa-onnx-v" + Version + "-"

// Release archive of the native library for each GOOS and GOARCH, pinned by size and sha256
var libraryArchives = map[string]fetch.Asset{
	"linux/amd64":   {Name: "linux-x64-shared-lib.tar.bz2", Size: 9816899, SHA256: "3892d184be41027e18165e67f549cd4e4cdd8dcd73ac5579e97afd55e14e30b6"},
	"linux/arm64":   {Name: "linux-aarch64-shared-cpu-lib.tar.bz2", Size: 12595068, SHA256: "fb98b80628a383909bf83626daca2e0d0ffc93bf1f7d222eaaa4282b388f072f"},
	"linux/riscv64": {Name: "linux-riscv64-shared.tar.bz2", Size: 15092445, SHA256: "56c379bdea52a785bd590ac7d4a70fc6806015375a014b957a0627e3bea628e5"},
	"darwin/arm64":  {Name: "osx-arm64-shared-lib.tar.bz2", Size: 8773198, SHA256: "ae77050cdae565496059d96f5ab33d77b397a0864282a4e4a26e3b3b3effb948"},
	"darwin/amd64":  {Name: "osx-x64-shared-lib.tar.bz2", Size: 10014197, SHA256: "0f88371565a06372889c76266e253abd1b415fcc6f98232cabd968933efb0711"},
	"windows/amd64": {Name: "win-x64-shared-MT-Release-lib.tar.bz2", Size: 8032957, SHA256: "b8eedf41bd6d3779218887b48367bb7a3ece5aaa7667f01f69ee823a12b0a9e7"},
	"windows/arm64": {Name: "win-arm64-shared-MT-Release-lib.tar.bz2", Size: 7622849, SHA256: "22cd2b2b5e35c1132abc74a91ee77f683645437382617af6a4bd5818b9c509f4"},
	"android/arm64": {Name: "android-aarch64-termux-shared.tar.bz2", Size: 16451109, SHA256: "ad7bac42adc6295293fe6d36475936afb227cdc757cf293c02a706f6aca4e0a2"},
}

// CUDA 12 builds of the native library, published for two targets
var gpuArchives = map[string]fetch.Asset{
	"linux/amd64":   {Name: "cuda-12.x-cudnn-9.x-onnxruntime1.28.2-linux-x64-gpu.tar.bz2", Size: 393010348, SHA256: "2132af1ae8c84c84f86ac091e4311839287851d2bf5e660332ebce3898a1e0b2"},
	"windows/amd64": {Name: "cuda-12.x-cudnn-9.x-onnxruntime1.28.2-win-x64-cuda.tar.bz2", Size: 595017373, SHA256: "066c5b54dbafaa1388001a9c9837ac1374dbba6d6678f193ca06aa0d8e94d8c3"},
}

// Returned on a platform the speech engine publishes no library for
type PlatformError struct {
	GPU bool
}

func (e *PlatformError) Error() string {
	if e.GPU {
		return "the local speech engine has no GPU build for " + runtime.GOOS + "/" + runtime.GOARCH
	}
	return "the local speech engine has no build for " + runtime.GOOS + "/" + runtime.GOARCH
}

// Returned when the process already runs the engine on the other kind of device
type DeviceError struct {
	Running string
	Asked   string
}

func (e *DeviceError) Error() string {
	return fmt.Sprintf("the local speech engine already runs on the %s in this process, restart icebath to move it to the %s", e.Running, e.Asked)
}

var loaded struct {
	sync.Mutex
	module *Module
}

func deviceName(gpu bool) string {
	if gpu {
		return "GPU"
	}
	return "CPU"
}

// Pinned archive of the native library for a platform and device, with its download address
func libraryArchive(goos, goarch string, gpu bool) (fetch.Asset, bool) {
	table := libraryArchives
	if gpu {
		table = gpuArchives
	}
	asset, ok := table[goos+"/"+goarch]
	if !ok {
		return fetch.Asset{}, false
	}
	asset.URL = releaseBase + asset.Name
	asset.Name = "sherpa-onnx-v" + Version + "-" + asset.Name
	return asset, true
}

// Whether the engine publishes a library for this platform
func Supported() bool {
	_, ok := libraryArchive(runtime.GOOS, runtime.GOARCH, false)
	return ok && fetch.NativeSupported
}

// Directory of the pinned native library package for this platform, fetched into the icebath cache on first use
func LibraryDir(ctx context.Context, gpu bool) (string, error) {
	asset, ok := libraryArchive(runtime.GOOS, runtime.GOARCH, gpu)
	if !ok || !fetch.NativeSupported {
		return "", &PlatformError{GPU: gpu}
	}
	tree, err := fetch.Tree(ctx, asset)
	if err != nil {
		return "", err
	}
	return filepath.Join(tree, strings.TrimSuffix(asset.Name, ".tar.bz2")), nil
}

// Module over the native library of this platform, one device kind per process
func Load(ctx context.Context, gpu bool) (*Module, error) {
	loaded.Lock()
	defer loaded.Unlock()
	if loaded.module != nil {
		if loaded.module.GPU != gpu {
			return nil, &DeviceError{Running: deviceName(loaded.module.GPU), Asked: deviceName(gpu)}
		}
		return loaded.module, nil
	}
	dir, err := LibraryDir(ctx, gpu)
	if err != nil {
		return nil, err
	}
	module, err := open(dir)
	if err != nil {
		return nil, fmt.Errorf("the local speech engine library failed to load: %w", err)
	}
	module.GPU = gpu
	loaded.module = module
	return module, nil
}

// The recognition, synthesis and voice activity functions of the sherpa-onnx C API
type speechAPI struct {
	createOfflineRecognizer  func(config *cOfflineRecognizerConfig) uintptr
	destroyOfflineRecognizer func(recognizer uintptr)
	createOfflineStream      func(recognizer uintptr) uintptr
	destroyOfflineStream     func(stream uintptr)
	acceptWaveformOffline    func(stream uintptr, sampleRate int32, samples *float32, n int32)
	decodeOfflineStream      func(recognizer, stream uintptr)
	getOfflineStreamResult   func(stream uintptr) uintptr
	destroyOfflineResult     func(result uintptr)
	createOfflineTts         func(config *cOfflineTtsConfig) uintptr
	destroyOfflineTts        func(tts uintptr)
	offlineTtsSampleRate     func(tts uintptr) int32
	offlineTtsNumSpeakers    func(tts uintptr) int32
	offlineTtsGenerate       func(tts uintptr, text *byte, config *cGenerationConfig, callback, arg uintptr) uintptr
	destroyGeneratedAudio    func(audio uintptr)
	createVAD                func(config *cVadModelConfig, bufferSeconds float32) uintptr
	destroyVAD               func(vad uintptr)
	vadAcceptWaveform        func(vad uintptr, samples *float32, n int32)
	vadEmpty                 func(vad uintptr) int32
	vadFront                 func(vad uintptr) uintptr
	vadPop                   func(vad uintptr)
	vadFlush                 func(vad uintptr)
	destroySpeechSegment     func(segment uintptr)
}

// Binds the speech functions of the loaded library once
func (m *Module) speechAPI() (*speechAPI, error) {
	m.speechOnce.Do(func() {
		api := &speechAPI{}
		for _, entry := range []struct {
			fptr any
			name string
		}{
			{&api.createOfflineRecognizer, "SherpaOnnxCreateOfflineRecognizer"}, {&api.destroyOfflineRecognizer, "SherpaOnnxDestroyOfflineRecognizer"},
			{&api.createOfflineStream, "SherpaOnnxCreateOfflineStream"}, {&api.destroyOfflineStream, "SherpaOnnxDestroyOfflineStream"},
			{&api.acceptWaveformOffline, "SherpaOnnxAcceptWaveformOffline"}, {&api.decodeOfflineStream, "SherpaOnnxDecodeOfflineStream"},
			{&api.getOfflineStreamResult, "SherpaOnnxGetOfflineStreamResult"}, {&api.destroyOfflineResult, "SherpaOnnxDestroyOfflineRecognizerResult"},
			{&api.createOfflineTts, "SherpaOnnxCreateOfflineTts"}, {&api.destroyOfflineTts, "SherpaOnnxDestroyOfflineTts"},
			{&api.offlineTtsSampleRate, "SherpaOnnxOfflineTtsSampleRate"}, {&api.offlineTtsNumSpeakers, "SherpaOnnxOfflineTtsNumSpeakers"},
			{&api.offlineTtsGenerate, "SherpaOnnxOfflineTtsGenerateWithConfig"}, {&api.destroyGeneratedAudio, "SherpaOnnxDestroyOfflineTtsGeneratedAudio"},
			{&api.createVAD, "SherpaOnnxCreateVoiceActivityDetector"}, {&api.destroyVAD, "SherpaOnnxDestroyVoiceActivityDetector"},
			{&api.vadAcceptWaveform, "SherpaOnnxVoiceActivityDetectorAcceptWaveform"}, {&api.vadEmpty, "SherpaOnnxVoiceActivityDetectorEmpty"},
			{&api.vadFront, "SherpaOnnxVoiceActivityDetectorFront"}, {&api.vadPop, "SherpaOnnxVoiceActivityDetectorPop"},
			{&api.vadFlush, "SherpaOnnxVoiceActivityDetectorFlush"}, {&api.destroySpeechSegment, "SherpaOnnxDestroySpeechSegment"},
		} {
			if err := m.lib.Bind(entry.fptr, entry.name); err != nil {
				m.speechErr = fmt.Errorf("the local speech engine library is incomplete: %w", err)
				return
			}
		}
		m.speech = api
	})
	return m.speech, m.speechErr
}

// Execution provider name the loaded library runs models on
func (m *Module) provider() string {
	if m.GPU {
		return "cuda"
	}
	return "cpu"
}

// NUL terminated copies of strings that stay alive as long as the arena does
type cStrings struct{ held []*byte }

func (c *cStrings) ptr(s string) *byte {
	if s == "" {
		return nil
	}
	buf := fetch.CString(s)
	c.held = append(c.held, buf)
	return buf
}

// Copies n float samples out of library memory
func copySamples(addr uintptr, n int32) []float32 {
	if addr == 0 || n <= 0 {
		return nil
	}
	return append([]float32(nil), unsafe.Slice(pointerAt[float32](addr), int(n))...)
}

func requireFiles(paths ...string) error {
	for _, path := range paths {
		if !isFile(path) {
			return fmt.Errorf("speech model file %s is missing", path)
		}
	}
	return nil
}

// Whisper model files and decoding choices of one recognizer
type WhisperOptions struct {
	Encoder  string
	Decoder  string
	Tokens   string
	Language string
	Threads  int
}

// A loaded offline speech recognizer
type Recognizer struct {
	api    *speechAPI
	mu     sync.Mutex
	handle uintptr
}

// Text and detected language of one decoded stretch of audio
type Transcript struct {
	Text     string
	Language string
}

// Loads a Whisper model for offline transcription, an empty language meaning automatic detection
func (m *Module) NewWhisper(o WhisperOptions) (*Recognizer, error) {
	api, err := m.speechAPI()
	if err != nil {
		return nil, err
	}
	if err := requireFiles(o.Encoder, o.Decoder, o.Tokens); err != nil {
		return nil, err
	}
	var strs cStrings
	config := &cOfflineRecognizerConfig{featConfig: cFeatureConfig{sampleRate: 16000, featureDim: 80}}
	config.modelConfig.whisper = cOfflineWhisperModelConfig{encoder: strs.ptr(o.Encoder), decoder: strs.ptr(o.Decoder),
		language: strs.ptr(o.Language), task: strs.ptr("transcribe"), tailPaddings: -1}
	config.modelConfig.tokens = strs.ptr(o.Tokens)
	config.modelConfig.numThreads = int32(max(o.Threads, 1))
	config.modelConfig.provider = strs.ptr(m.provider())
	config.decodingMethod = strs.ptr("greedy_search")
	handle := api.createOfflineRecognizer(config)
	runtime.KeepAlive(strs)
	if handle == 0 {
		return nil, fmt.Errorf("the speech engine rejected the Whisper model at %s", o.Encoder)
	}
	return &Recognizer{api: api, handle: handle}, nil
}

// Transcribes up to thirty seconds of mono audio
func (r *Recognizer) Decode(sampleRate int, samples []float32) (Transcript, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 {
		return Transcript{}, errors.New("the speech recognizer is closed")
	}
	if len(samples) == 0 {
		return Transcript{}, nil
	}
	stream := r.api.createOfflineStream(r.handle)
	defer r.api.destroyOfflineStream(stream)
	r.api.acceptWaveformOffline(stream, int32(sampleRate), &samples[0], int32(len(samples)))
	r.api.decodeOfflineStream(r.handle, stream)
	runtime.KeepAlive(samples)
	result := r.api.getOfflineStreamResult(stream)
	if result == 0 {
		return Transcript{}, errors.New("the speech engine returned no result")
	}
	defer r.api.destroyOfflineResult(result)
	res := pointerAt[cOfflineRecognizerResult](result)
	return Transcript{Text: goString(res.text), Language: goString(res.lang)}, nil
}

// Releases the model
func (r *Recognizer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle != 0 {
		r.api.destroyOfflineRecognizer(r.handle)
		r.handle = 0
	}
}

func goString(p *byte) string {
	if p == nil {
		return ""
	}
	return fetch.GoString(uintptr(unsafe.Pointer(p)))
}

// Silero voice activity model and the pauses that end a stretch of speech
type VADOptions struct {
	Model      string
	MinSilence float32
	MaxSpeech  float32
}

// One stretch of speech with the index of its first sample
type Segment struct {
	Start   int
	Samples []float32
}

// Splits 16 kHz mono audio into its stretches of speech
func (m *Module) SpeechSegments(o VADOptions, samples []float32) ([]Segment, error) {
	api, err := m.speechAPI()
	if err != nil {
		return nil, err
	}
	if err := requireFiles(o.Model); err != nil {
		return nil, err
	}
	const window = 512
	var strs cStrings
	config := &cVadModelConfig{sampleRate: 16000, numThreads: 1, provider: strs.ptr("cpu")}
	config.sileroVad = cSileroVadModelConfig{model: strs.ptr(o.Model), threshold: 0.5, minSilenceDuration: o.MinSilence,
		minSpeechDuration: 0.25, windowSize: window, maxSpeechDuration: o.MaxSpeech}
	vad := api.createVAD(config, float32(len(samples))/16000+o.MaxSpeech+1)
	runtime.KeepAlive(strs)
	if vad == 0 {
		return nil, fmt.Errorf("the speech engine rejected the voice activity model at %s", o.Model)
	}
	defer api.destroyVAD(vad)
	var out []Segment
	drain := func() {
		for api.vadEmpty(vad) == 0 {
			front := api.vadFront(vad)
			seg := pointerAt[cSpeechSegment](front)
			out = append(out, Segment{Start: int(seg.start), Samples: copySamples(seg.samples, seg.n)})
			api.destroySpeechSegment(front)
			api.vadPop(vad)
		}
	}
	for at := 0; at+window <= len(samples); at += window {
		api.vadAcceptWaveform(vad, &samples[at], window)
		drain()
	}
	api.vadFlush(vad)
	drain()
	runtime.KeepAlive(samples)
	return out, nil
}

// Model files of one offline voice, Voices set for the Kitten family and unset for VITS voices such as Piper
type VoiceOptions struct {
	Model       string
	Tokens      string
	DataDir     string
	Voices      string
	NoiseScale  float32
	NoiseScaleW float32
	LengthScale float32
	Threads     int
}

// A loaded offline text to speech voice
type Voice struct {
	api    *speechAPI
	mu     sync.Mutex
	handle uintptr
}

// Loads a VITS or Kitten voice for offline synthesis
func (m *Module) NewVoice(o VoiceOptions) (*Voice, error) {
	api, err := m.speechAPI()
	if err != nil {
		return nil, err
	}
	files := []string{o.Model, o.Tokens}
	if o.Voices != "" {
		files = append(files, o.Voices)
	}
	if err := requireFiles(files...); err != nil {
		return nil, err
	}
	var strs cStrings
	config := &cOfflineTtsConfig{maxNumSentences: 1, silenceScale: 0.2}
	config.model.numThreads = int32(max(o.Threads, 1))
	config.model.provider = strs.ptr(m.provider())
	if o.Voices != "" {
		config.model.kitten = cOfflineTtsKittenModelConfig{model: strs.ptr(o.Model), voices: strs.ptr(o.Voices), tokens: strs.ptr(o.Tokens),
			dataDir: strs.ptr(o.DataDir), lengthScale: o.LengthScale}
	} else {
		config.model.vits = cOfflineTtsVitsModelConfig{model: strs.ptr(o.Model), tokens: strs.ptr(o.Tokens), dataDir: strs.ptr(o.DataDir),
			noiseScale: o.NoiseScale, noiseScaleW: o.NoiseScaleW, lengthScale: o.LengthScale}
	}
	handle := api.createOfflineTts(config)
	runtime.KeepAlive(strs)
	if handle == 0 {
		return nil, fmt.Errorf("the speech engine rejected the voice model at %s", o.Model)
	}
	return &Voice{api: api, handle: handle}, nil
}

// Number of speakers the voice model holds
func (v *Voice) Speakers() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.handle == 0 {
		return 0
	}
	return int(v.api.offlineTtsNumSpeakers(v.handle))
}

// Synthesizes text as mono float samples with their sample rate
func (v *Voice) Speak(text string, speaker int, speed float32) ([]float32, int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.handle == 0 {
		return nil, 0, errors.New("the voice is closed")
	}
	var strs cStrings
	config := &cGenerationConfig{silenceScale: 0.2, speed: speed, sid: int32(speaker)}
	audio := v.api.offlineTtsGenerate(v.handle, strs.ptr(text), config, 0, 0)
	runtime.KeepAlive(strs)
	if audio == 0 {
		return nil, 0, errors.New("the speech engine produced no audio")
	}
	defer v.api.destroyGeneratedAudio(audio)
	res := pointerAt[cGeneratedAudio](audio)
	return copySamples(res.samples, res.n), int(res.sampleRate), nil
}

// Releases the voice model
func (v *Voice) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.handle != 0 {
		v.api.destroyOfflineTts(v.handle)
		v.handle = 0
	}
}
