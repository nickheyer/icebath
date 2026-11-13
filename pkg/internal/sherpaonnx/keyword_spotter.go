package sherpaonnx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/fetch"
)

// Model files and decoding choices of one keyword spotter
type KeywordSpotterOptions struct {
	Tokens            string
	Encoder           string
	Decoder           string
	Joiner            string
	KeywordsFile      string
	NumThreads        int
	KeywordsThreshold float64
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
	if len(samples) == 0 {
		return
	}
	s.api.onlineStreamAcceptWaveform(s.handle, int32(sampleRate), &samples[0], int32(len(samples)))
	runtime.KeepAlive(samples)
	runtime.KeepAlive(s)
}

// Spots keywords with a streaming transducer model and a keyword list
type KeywordSpotter struct {
	api    *capi
	handle uintptr
}

// Builds the spotter after checking the model files exist and the thread count is positive
func (m *Module) NewKeywordSpotter(o KeywordSpotterOptions) (*KeywordSpotter, error) {
	for _, f := range []string{o.Tokens, o.Encoder, o.Decoder, o.Joiner, o.KeywordsFile} {
		if info, err := os.Stat(f); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("keyword spotting file %s is missing", f)
		}
	}
	if o.NumThreads <= 0 || o.KeywordsThreshold <= 0 {
		return nil, errors.New("keyword spotting needs a positive thread count and threshold")
	}
	config := &cKeywordSpotterConfig{
		featConfig: cFeatureConfig{sampleRate: 16000, featureDim: 80},
		modelConfig: cOnlineModelConfig{transducerEncoder: fetch.CString(o.Encoder), transducerDecoder: fetch.CString(o.Decoder),
			transducerJoiner: fetch.CString(o.Joiner), tokens: fetch.CString(o.Tokens), numThreads: int32(o.NumThreads), provider: fetch.CString("cpu")},
		maxActivePaths: 4, numTrailingBlanks: 1, keywordsScore: 1.0, keywordsThreshold: float32(o.KeywordsThreshold),
		keywordsFile: fetch.CString(o.KeywordsFile),
	}
	handle := m.api.createKeywordSpotter(config)
	runtime.KeepAlive(config)
	if handle == 0 {
		return nil, fmt.Errorf("the speech engine rejected the keyword spotting model at %s", o.Encoder)
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

// New stream for keyword spotting over the keywords file
func (k *KeywordSpotter) CreateStream() *OnlineStream {
	handle := k.api.createKeywordStream(k.handle)
	runtime.KeepAlive(k)
	return newOnlineStream(k.api, handle)
}

// Decodes one step for the stream
func (k *KeywordSpotter) DecodeStream(s *OnlineStream) {
	k.api.decodeKeywordStream(k.handle, s.handle)
	runtime.KeepAlive(k)
	runtime.KeepAlive(s)
}

// Whether the stream has enough frames for decoding
func (k *KeywordSpotter) IsReady(s *OnlineStream) bool {
	ready := k.api.isKeywordStreamReady(k.handle, s.handle) != 0
	runtime.KeepAlive(k)
	runtime.KeepAlive(s)
	return ready
}

// Detected keyword of the stream, empty when none is detected
func (k *KeywordSpotter) GetResult(s *OnlineStream) string {
	r := k.api.getKeywordResult(k.handle, s.handle)
	runtime.KeepAlive(k)
	runtime.KeepAlive(s)
	if r == 0 {
		return ""
	}
	keyword := readKeyword(r)
	k.api.destroyKeywordResult(r)
	return strings.TrimSpace(keyword)
}

// Token type of a keyword model directory with the BPE model or phone lexicon it needs
func KeywordTokenOptions(dir string) (Text2TokenOptions, error) {
	if bpe := filepath.Join(dir, "bpe.model"); isFile(bpe) {
		return Text2TokenOptions{TokensType: "bpe", BPEModel: bpe}, nil
	}
	if lexicon := filepath.Join(dir, "en.phone"); isFile(lexicon) {
		return Text2TokenOptions{TokensType: "phone+ppinyin", Lexicon: lexicon}, nil
	}
	table, err := readTokenTable(filepath.Join(dir, "tokens.txt"))
	if err != nil {
		return Text2TokenOptions{}, err
	}
	cjk := false
	for token := range table {
		if hasToneMark(token) {
			return Text2TokenOptions{TokensType: "ppinyin"}, nil
		}
		cjk = cjk || allCJK(token)
	}
	if cjk {
		return Text2TokenOptions{TokensType: "cjkchar"}, nil
	}
	return Text2TokenOptions{}, fmt.Errorf("the keyword model at %s has no bpe.model or en.phone and its tokens are neither pinyin nor Chinese characters", dir)
}
