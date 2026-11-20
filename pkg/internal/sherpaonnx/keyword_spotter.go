package sherpaonnx

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// The constructor arguments of the keyword spotter
type KeywordSpotterOptions struct {
	Tokens            string
	Encoder           string
	Decoder           string
	Joiner            string
	KeywordsFile      string
	NumThreads        int
	SampleRate        int
	FeatureDim        int
	MaxActivePaths    int
	KeywordsScore     float64
	KeywordsThreshold float64
	NumTrailingBlanks int
	Provider          string
	Device            int
}

// Constructor arguments at their Python defaults for the given model files
func DefaultKeywordSpotterOptions(tokens, encoder, decoder, joiner, keywordsFile string) KeywordSpotterOptions {
	return KeywordSpotterOptions{Tokens: tokens, Encoder: encoder, Decoder: decoder, Joiner: joiner, KeywordsFile: keywordsFile, NumThreads: 2,
		SampleRate: 16000, FeatureDim: 80, MaxActivePaths: 4, KeywordsScore: 1.0, KeywordsThreshold: 0.25, NumTrailingBlanks: 1, Provider: "cpu"}
}

func assertFileExists(f string) error {
	info, err := os.Stat(f)
	if err != nil || !info.Mode().IsRegular() {
		return &PyError{Class: "AssertionError", Msg: f + " does not exist"}
	}
	return nil
}

// One decoding stream of a keyword spotter
type OnlineStream struct {
	api    *capi
	handle uintptr
}

func newOnlineStream(api *capi, handle uintptr) *OnlineStream {
	s := &OnlineStream{api: api, handle: handle}
	runtime.SetFinalizer(s, func(s *OnlineStream) { s.api.destroyOnlineStream(s.handle) })
	return s
}

// Feeds audio samples in the range -1 to 1 at the given sample rate
func (s *OnlineStream) AcceptWaveform(sampleRate int, samples []float32) {
	padded := append(make([]float32, 0, len(samples)+1), samples...)
	s.api.onlineStreamAcceptWaveform(s.handle, int32(sampleRate), unsafe.SliceData(padded), int32(len(samples)))
	runtime.KeepAlive(s)
}

// Marks the end of the audio so the remaining frames decode
func (s *OnlineStream) InputFinished() {
	s.api.onlineStreamInputFinished(s.handle)
	runtime.KeepAlive(s)
}

// Spots keywords with a streaming transducer model and a keyword list
type KeywordSpotter struct {
	api    *capi
	handle uintptr
}

// The C API replaces zero with a default, so a zero the Python layer would pass through is refused
func refuseZero(name string, zero bool) error {
	if zero {
		return &PyError{Class: "ValueError", Msg: name + "=0 is replaced by the library default in the sherpa-onnx C API"}
	}
	return nil
}

// Builds the spotter after checking the model files exist and the thread count is positive
func (m *Module) NewKeywordSpotter(o KeywordSpotterOptions) (*KeywordSpotter, error) {
	for _, f := range []string{o.Tokens, o.Encoder, o.Decoder, o.Joiner} {
		if err := assertFileExists(f); err != nil {
			return nil, err
		}
	}
	if o.NumThreads <= 0 {
		return nil, &PyError{Class: "AssertionError", Msg: fmt.Sprint(o.NumThreads)}
	}
	for _, check := range []struct {
		name string
		zero bool
	}{{"sample_rate", o.SampleRate == 0}, {"feature_dim", o.FeatureDim == 0}, {"max_active_paths", o.MaxActivePaths == 0},
		{"keywords_score", o.KeywordsScore == 0}, {"keywords_threshold", o.KeywordsThreshold == 0},
		{"num_trailing_blanks", o.NumTrailingBlanks == 0}} {
		if err := refuseZero(check.name, check.zero); err != nil {
			return nil, err
		}
	}
	if o.Device != 0 {
		return nil, &PyError{Class: "ValueError", Msg: fmt.Sprintf("device=%d has no field in the sherpa-onnx C API keyword spotter config", o.Device)}
	}
	strs := [][]byte{pyctypes.CString(o.Encoder), pyctypes.CString(o.Decoder), pyctypes.CString(o.Joiner), pyctypes.CString(o.Tokens),
		pyctypes.CString(o.Provider), pyctypes.CString(o.KeywordsFile)}
	config := &cKeywordSpotterConfig{
		featConfig: cFeatureConfig{sampleRate: int32(o.SampleRate), featureDim: int32(o.FeatureDim)},
		modelConfig: cOnlineModelConfig{transducerEncoder: &strs[0][0], transducerDecoder: &strs[1][0], transducerJoiner: &strs[2][0],
			tokens: &strs[3][0], numThreads: int32(o.NumThreads), provider: &strs[4][0]},
		maxActivePaths: int32(o.MaxActivePaths), numTrailingBlanks: int32(o.NumTrailingBlanks),
		keywordsScore: float32(o.KeywordsScore), keywordsThreshold: float32(o.KeywordsThreshold), keywordsFile: &strs[5][0],
	}
	handle := m.api.createKeywordSpotter(config)
	runtime.KeepAlive(strs)
	if handle == 0 {
		return nil, &PyError{Class: "RuntimeError", Msg: "Errors in config!"}
	}
	k := &KeywordSpotter{api: m.api, handle: handle}
	runtime.SetFinalizer(k, func(k *KeywordSpotter) { k.api.destroyKeywordSpotter(k.handle) })
	return k, nil
}

// Resets the stream after a keyword is detected and before more audio is fed
func (k *KeywordSpotter) ResetStream(s *OnlineStream) {
	k.api.resetKeywordStream(k.handle, s.handle)
	runtime.KeepAlive(k)
	runtime.KeepAlive(s)
}

// New stream for keyword spotting, with extra keywords in the keywords file format when keywords is not nil
func (k *KeywordSpotter) CreateStream(keywords *string) *OnlineStream {
	var handle uintptr
	if keywords == nil {
		handle = k.api.createKeywordStream(k.handle)
	} else {
		handle = k.api.createKeywordStreamWithKeywords(k.handle, &pyctypes.CString(*keywords)[0])
	}
	runtime.KeepAlive(k)
	return newOnlineStream(k.api, handle)
}

// Decodes one step for the stream
func (k *KeywordSpotter) DecodeStream(s *OnlineStream) {
	k.api.decodeKeywordStream(k.handle, s.handle)
	runtime.KeepAlive(k)
	runtime.KeepAlive(s)
}

// Decodes one step on several streams at the same time
func (k *KeywordSpotter) DecodeStreams(ss []*OnlineStream) {
	handles := make([]uintptr, len(ss)+1)
	for i, s := range ss {
		handles[i] = s.handle
	}
	k.api.decodeMultipleKeywordStreams(k.handle, &handles[0], int32(len(ss)))
	runtime.KeepAlive(k)
	runtime.KeepAlive(ss)
}

// Whether the stream has enough frames for decoding
func (k *KeywordSpotter) IsReady(s *OnlineStream) bool {
	ready := k.api.isKeywordStreamReady(k.handle, s.handle) != 0
	runtime.KeepAlive(k)
	runtime.KeepAlive(s)
	return ready
}

// Keyword, tokens and token timestamps of the current result of the stream
func (k *KeywordSpotter) result(s *OnlineStream) (string, []string, []float32) {
	r := k.api.getKeywordResult(k.handle, s.handle)
	runtime.KeepAlive(k)
	runtime.KeepAlive(s)
	if r == 0 {
		return "", []string{}, []float32{}
	}
	keyword, tokens, timestamps := readKeywordResult(r)
	k.api.destroyKeywordResult(r)
	return keyword, tokens, timestamps
}

// Detected keyword of the stream, empty when none is detected
func (k *KeywordSpotter) GetResult(s *OnlineStream) string {
	keyword, _, _ := k.result(s)
	return pystr.Strip(keyword)
}

// Token list of the keyword result
func (k *KeywordSpotter) Tokens(s *OnlineStream) []string {
	_, tokens, _ := k.result(s)
	return tokens
}

// Timestamp in seconds of each token of the keyword result
func (k *KeywordSpotter) Timestamps(s *OnlineStream) []float32 {
	_, _, timestamps := k.result(s)
	return timestamps
}
