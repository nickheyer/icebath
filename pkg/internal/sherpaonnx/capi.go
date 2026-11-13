package sherpaonnx

import (
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/fetch"
)

// Release of sherpa-onnx whose native library this binding loads
const Version = "1.13.8"

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
	createKeywordSpotter       func(config *cKeywordSpotterConfig) uintptr
	destroyKeywordSpotter      func(spotter uintptr)
	createKeywordStream        func(spotter uintptr) uintptr
	isKeywordStreamReady       func(spotter, stream uintptr) int32
	decodeKeywordStream        func(spotter, stream uintptr)
	resetKeywordStream         func(spotter, stream uintptr)
	getKeywordResult           func(spotter, stream uintptr) uintptr
	destroyKeywordResult       func(result uintptr)
	destroyOnlineStream        func(stream uintptr)
	onlineStreamAcceptWaveform func(stream uintptr, sampleRate int32, samples *float32, n int32)
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

// The loaded sherpa-onnx library and the directory of its package
type Module struct {
	Dir string
	GPU bool
	api *capi
	lib *fetch.Library

	speechOnce sync.Once
	speech     *speechAPI
	speechErr  error
}

// Binds the native library of the package unpacked at dir
func open(dir string) (*Module, error) {
	lib, err := fetch.OpenLibrary(filepath.Join(dir, "lib", libraryName(runtime.GOOS)))
	if err != nil {
		return nil, err
	}
	api := &capi{}
	if err := lib.BindAll([]fetch.Binding{
		{&api.createKeywordSpotter, "SherpaOnnxCreateKeywordSpotter"}, {&api.destroyKeywordSpotter, "SherpaOnnxDestroyKeywordSpotter"},
		{&api.createKeywordStream, "SherpaOnnxCreateKeywordStream"},
		{&api.isKeywordStreamReady, "SherpaOnnxIsKeywordStreamReady"}, {&api.decodeKeywordStream, "SherpaOnnxDecodeKeywordStream"},
		{&api.resetKeywordStream, "SherpaOnnxResetKeywordStream"},
		{&api.getKeywordResult, "SherpaOnnxGetKeywordResult"}, {&api.destroyKeywordResult, "SherpaOnnxDestroyKeywordResult"},
		{&api.destroyOnlineStream, "SherpaOnnxDestroyOnlineStream"}, {&api.onlineStreamAcceptWaveform, "SherpaOnnxOnlineStreamAcceptWaveform"},
	}); err != nil {
		return nil, err
	}
	return &Module{Dir: dir, api: api, lib: lib}, nil
}

// Keyword text of a keyword result
func readKeyword(r uintptr) string {
	return fetch.GoString(fetch.Deref[cKeywordResult](r).keyword)
}

func pointerAt[T any](addr uintptr) *T { return (*T)(*(*unsafe.Pointer)(unsafe.Pointer(&addr))) }
