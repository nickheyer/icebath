package sherpaonnx

import (
	"runtime"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Release of the sherpa_onnx package this binding ports
const Version = "1.13.8"

// A Python exception the package raises, named by its class
type PyError struct{ Class, Msg string }

func (e *PyError) Error() string { return e.Msg }

// SherpaOnnxFeatureConfig of the C API
type cFeatureConfig struct {
	sampleRate int32
	featureDim int32
}

// SherpaOnnxOnlineModelConfig of the C API with its nested model structs flattened in declaration order
type cOnlineModelConfig struct {
	transducerEncoder  *byte
	transducerDecoder  *byte
	transducerJoiner   *byte
	paraformerEncoder  *byte
	paraformerDecoder  *byte
	zipformer2CtcModel *byte
	tokens             *byte
	numThreads         int32
	provider           *byte
	debug              int32
	modelType          *byte
	modelingUnit       *byte
	bpeVocab           *byte
	tokensBuf          *byte
	tokensBufSize      int32
	nemoCtcModel       *byte
	tOneCtcModel       *byte
}

// SherpaOnnxKeywordSpotterConfig of the C API
type cKeywordSpotterConfig struct {
	featConfig        cFeatureConfig
	modelConfig       cOnlineModelConfig
	maxActivePaths    int32
	numTrailingBlanks int32
	keywordsScore     float32
	keywordsThreshold float32
	keywordsFile      *byte
	keywordsBuf       *byte
	keywordsBufSize   int32
}

// SherpaOnnxKeywordResult of the C API
type cKeywordResult struct {
	keyword    uintptr
	tokens     uintptr
	tokensArr  uintptr
	count      int32
	timestamps uintptr
	startTime  float32
	json       uintptr
}

// The keyword spotting functions of the sherpa-onnx C API
type capi struct {
	createKeywordSpotter            func(config *cKeywordSpotterConfig) uintptr
	destroyKeywordSpotter           func(spotter uintptr)
	createKeywordStream             func(spotter uintptr) uintptr
	createKeywordStreamWithKeywords func(spotter uintptr, keywords *byte) uintptr
	isKeywordStreamReady            func(spotter, stream uintptr) int32
	decodeKeywordStream             func(spotter, stream uintptr)
	resetKeywordStream              func(spotter, stream uintptr)
	decodeMultipleKeywordStreams    func(spotter uintptr, streams *uintptr, n int32)
	getKeywordResult                func(spotter, stream uintptr) uintptr
	destroyKeywordResult            func(result uintptr)
	destroyOnlineStream             func(stream uintptr)
	onlineStreamAcceptWaveform      func(stream uintptr, sampleRate int32, samples *float32, n int32)
	onlineStreamInputFinished       func(stream uintptr)
}

// File name of the C API library inside the package lib directory on a GOOS
func libraryName(goos string) string {
	switch goos {
	case "windows":
		return "sherpa-onnx-c-api.dll"
	case "darwin":
		return "libsherpa-onnx-c-api.dylib"
	}
	return "libsherpa-onnx-c-api.so"
}

// The imported sherpa_onnx package, its directory and the bound native library
type Module struct {
	Dir string
	GPU bool
	api *capi
	env Environment
	lib *pyctypes.CDLL

	speechOnce sync.Once
	speech     *speechAPI
	speechErr  error
}

// Imports the package installed at dir, an ImportError when its native library cannot be loaded or lacks a function
func Import(dir string, env Environment) (*Module, error) {
	path := pypath.Join(dir, "lib", libraryName(runtime.GOOS))
	cdll, err := pyctypes.LoadLibrary(path, false)
	if err != nil {
		return nil, &PyError{Class: "ImportError", Msg: err.Error()}
	}
	api := &capi{}
	for _, entry := range []struct {
		fptr any
		name string
	}{
		{&api.createKeywordSpotter, "SherpaOnnxCreateKeywordSpotter"}, {&api.destroyKeywordSpotter, "SherpaOnnxDestroyKeywordSpotter"},
		{&api.createKeywordStream, "SherpaOnnxCreateKeywordStream"},
		{&api.createKeywordStreamWithKeywords, "SherpaOnnxCreateKeywordStreamWithKeywords"},
		{&api.isKeywordStreamReady, "SherpaOnnxIsKeywordStreamReady"}, {&api.decodeKeywordStream, "SherpaOnnxDecodeKeywordStream"},
		{&api.resetKeywordStream, "SherpaOnnxResetKeywordStream"}, {&api.decodeMultipleKeywordStreams, "SherpaOnnxDecodeMultipleKeywordStreams"},
		{&api.getKeywordResult, "SherpaOnnxGetKeywordResult"}, {&api.destroyKeywordResult, "SherpaOnnxDestroyKeywordResult"},
		{&api.destroyOnlineStream, "SherpaOnnxDestroyOnlineStream"}, {&api.onlineStreamAcceptWaveform, "SherpaOnnxOnlineStreamAcceptWaveform"},
		{&api.onlineStreamInputFinished, "SherpaOnnxOnlineStreamInputFinished"},
	} {
		addr, err := cdll.Symbol(entry.name)
		if err != nil {
			return nil, &PyError{Class: "ImportError", Msg: err.Error()}
		}
		pyctypes.RegisterFunc(entry.fptr, addr)
	}
	return &Module{Dir: dir, api: api, env: env, lib: cdll}, nil
}

// Copies of the fields of a keyword result the Python result object exposes
func readKeywordResult(r uintptr) (keyword string, tokens []string, timestamps []float32) {
	res := (*cKeywordResult)(*(*unsafe.Pointer)(unsafe.Pointer(&r)))
	keyword = pyctypes.GoString(res.keyword)
	tokens = make([]string, 0, res.count)
	timestamps = []float32{}
	for i := 0; i < int(res.count); i++ {
		tokens = append(tokens, pyctypes.GoString(pyctypes.PointerAt(res.tokensArr, i)))
	}
	if res.timestamps != 0 {
		stamps := res.timestamps
		timestamps = append(timestamps, unsafe.Slice((*float32)(*(*unsafe.Pointer)(unsafe.Pointer(&stamps))), int(res.count))...)
	}
	return keyword, tokens, timestamps
}
