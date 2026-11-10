package media

import (
	"compress/bzip2"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	wakeImportPyOpenWakeWord = importPyOpenWakeWord
	wakeImportSherpaOnnx     = importSherpaOnnx
	wakeImportPvporcupine    = importPvporcupine
)

// WakeEngine is the hotword engine contract, int16 frames in and a fire verdict out
type WakeEngine interface {
	FrameLength() int
	Process(frame []int16) (bool, error)
	Reset() error
	Close()
}

// Engine defaults shared by every engine, an 80 ms frame at 16 kHz with nothing to reset or release
type wakeEngineBase struct {
	frameLength int
}

func (b *wakeEngineBase) FrameLength() int { return b.frameLength }
func (b *wakeEngineBase) Reset() error     { return nil }
func (b *wakeEngineBase) Close()           {}

// Ensures the engine extra, and the audio capture extra only when capture is local
func wakeEnsureDep(ctx context.Context, feature string, cfg map[string]any) error {
	if err := wakeEnsureImport(ctx, feature); err != nil {
		return err
	}
	if ResolveCaptureMode(ctx, cfg, false, false) != "local" {
		return nil
	}
	available, err := wakePMAvailable(ctx, "audio-io")
	if err != nil || available {
		return err
	}
	return wakeEnsureImport(ctx, "audio-io")
}

// Whether a model reference names a file rather than a built in keyword
func looksLikePath(value string) bool {
	if strings.Contains(value, string(os.PathSeparator)) {
		return true
	}
	for _, ext := range []string{".onnx", ".tflite", ".ppn"} {
		if strings.HasSuffix(value, ext) {
			return true
		}
	}
	_, err := os.Stat(value)
	return value != "" && err == nil
}

// Engine sub section of the wake config, empty unless a mapping
func wakeSub(cfg map[string]any, key string) map[string]any {
	if sub, ok := asStrMap(cfg[key]); ok {
		return sub
	}
	return map[string]any{}
}

// The openWakeWord feature pipeline pyopen-wakeword exposes
type owwFeatures interface {
	ProcessStreaming(audioChunk []byte, yield func(embedding []float32) bool) error
	Reset()
	Close()
}

// One openWakeWord classifier pyopen-wakeword exposes
type owwModel interface {
	ID() string
	ProcessStreaming(embedding []float32, yield func(score float32) bool) error
	Reset()
	Close()
}

// The pyopen_wakeword names the engine imports
type pyOpenWakeWordModule interface {
	FeaturesFromBuiltin() (owwFeatures, error)
	ModelFromModel(modelPath string) (owwModel, error)
}

// openWakeWord engine over pyopen-wakeword, sensitivity as the raw threshold with a confirmation streak
type openWakeWordEngine struct {
	wakeEngineBase
	threshold     float64
	confirmNeeded int
	confirmStreak int
	features      owwFeatures
	model         owwModel
	labels        []string
}

// Builds the openWakeWord engine on the bundled hey_hermes model unless a custom model is configured
func newOpenWakeWordEngine(ctx context.Context, cfg map[string]any) (WakeEngine, error) {
	if err := wakeEnsureDep(ctx, "wake-openwakeword", cfg); err != nil {
		return nil, err
	}
	sub := wakeSub(cfg, "openwakeword")
	lib, err := wakeImportPyOpenWakeWord(ctx)
	if err != nil {
		return nil, err
	}
	ref := any(wakeBundledModelName)
	if pyTruthy(sub["model"]) {
		ref = sub["model"]
	}
	modelRef := pystr.Strip(pyrepr.Str(ref))
	if wakeBundledModelAliases[pystr.Lower(modelRef)] {
		if modelRef, err = bundledWakewordPath(ctx); err != nil {
			return nil, err
		}
	}
	e := &openWakeWordEngine{wakeEngineBase: wakeEngineBase{frameLength: 1280}}
	if e.threshold, err = wakeSensitivity(cfg); err != nil {
		return nil, err
	}
	if e.confirmNeeded, err = wakeConfirmationFrames(cfg); err != nil {
		return nil, err
	}
	if e.features, err = lib.FeaturesFromBuiltin(); err != nil {
		return nil, err
	}
	if e.model, err = lib.ModelFromModel(modelRef); err != nil {
		e.features.Close()
		return nil, err
	}
	e.labels = []string{e.model.ID()}
	return e, nil
}

// Scores one frame and fires after the configured run of consecutive over threshold frames
func (e *openWakeWordEngine) Process(frame []int16) (bool, error) {
	chunk := make([]byte, 2*len(frame))
	for i, v := range frame {
		binary.NativeEndian.PutUint16(chunk[2*i:], uint16(v))
	}
	over := false
	var modelErr error
	err := e.features.ProcessStreaming(chunk, func(emb []float32) bool {
		modelErr = e.model.ProcessStreaming(emb, func(score float32) bool {
			if float64(score) >= e.threshold {
				over = true
			}
			return true
		})
		return modelErr == nil
	})
	if modelErr != nil {
		return false, modelErr
	}
	if err != nil {
		return false, err
	}
	if !over {
		e.confirmStreak = 0
		return false, nil
	}
	e.confirmStreak++
	if e.confirmStreak >= e.confirmNeeded {
		e.confirmStreak = 0
		return true, nil
	}
	return false, nil
}

// Clears the rolling feature and prediction buffers so audio from before a pause cannot fire
func (e *openWakeWordEngine) Reset() error {
	e.confirmStreak = 0
	e.features.Reset()
	e.model.Reset()
	return nil
}

func (e *openWakeWordEngine) Close() {
	_ = e.Reset()
	e.features.Close()
	e.model.Close()
}

const (
	sherpaKWSModelURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/kws-models/" +
		"sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01.tar.bz2"
	sherpaKWSModelDir = "sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01"
)

func sherpaModelRoot(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "cache", "wakewords")
}

// Client the one time sherpa model download goes through
var sherpaModelHTTP = &http.Client{Timeout: 10 * time.Minute}

// Downloads and unpacks the sherpa KWS model once, returning its directory
func ensureSherpaModel(ctx context.Context, root string) (string, error) {
	if root == "" {
		root = sherpaModelRoot(ctx)
	}
	target := filepath.Join(root, sherpaKWSModelDir)
	if _, err := os.Stat(filepath.Join(target, "tokens.txt")); err == nil {
		return target, nil
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", err
	}
	archive := filepath.Join(root, sherpaKWSModelDir+".tar.bz2")
	wakeLog().Info("wake word: downloading sherpa KWS model (one-time, ~13 MB)")
	if err := urlRetrieve(ctx, sherpaKWSModelURL, archive); err != nil {
		return "", err
	}
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	err = pyfs.ExtractTarDataReader(bzip2.NewReader(f), root)
	f.Close()
	if err != nil {
		return "", err
	}
	if err := os.Remove(archive); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(target, "tokens.txt")); err != nil {
		return "", &WakeWordError{Class: "RuntimeError", Msg: "sherpa KWS model unpack failed: " + target}
	}
	return target, nil
}

// Copies a URL to a file like urllib.request.urlretrieve, an HTTP error status failing the copy
func urlRetrieve(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := sherpaModelHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &WakeWordError{Class: "HTTPError", Msg: fmt.Sprintf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))}
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Keyword spotter config sherpa_onnx.KeywordSpotter takes
type sherpaKWSConfig struct {
	Tokens, Encoder, Decoder, Joiner, KeywordsFile string
	KeywordsThreshold                              float64
	NumThreads                                     int
}

// One sherpa decoding stream
type sherpaStream interface {
	AcceptWaveform(sampleRate int, samples []float32) error
}

// The sherpa_onnx.KeywordSpotter methods the engine drives
type sherpaKeywordSpotter interface {
	CreateStream() (sherpaStream, error)
	IsReady(s sherpaStream) bool
	DecodeStream(s sherpaStream) error
	GetResult(s sherpaStream) string
	ResetStream(s sherpaStream) error
}

// The sherpa_onnx names the engine imports
type sherpaOnnxModule interface {
	Text2Token(texts []string, tokens, tokensType, bpeModel string) ([][]string, error)
	KeywordSpotter(cfg sherpaKWSConfig) (sherpaKeywordSpotter, error)
}

// Open vocabulary sherpa keyword spotter listening for every enrolled profile phrase
type sherpaKwsEngine struct {
	wakeEngineBase
	displayToProfile map[string]string
	keywordsFile     string
	spotter          sherpaKeywordSpotter
	stream           sherpaStream
	matchMu          sync.Mutex
	lastMatch        *[2]string
}

// Builds the sherpa engine, tokenizing every phrase against the model vocabulary into a keywords file
func newSherpaKwsEngine(ctx context.Context, cfg map[string]any) (WakeEngine, error) {
	if err := wakeEnsureDep(ctx, "wake-sherpa", cfg); err != nil {
		return nil, err
	}
	sub := wakeSub(cfg, "sherpa")
	sherpa, err := wakeImportSherpaOnnx(ctx)
	if err != nil {
		return nil, err
	}
	modelDir := ""
	if pyTruthy(sub["model_dir"]) {
		modelDir = pyrepr.Str(sub["model_dir"])
	}
	d := pystr.Strip(modelDir)
	if d == "" {
		if d, err = ensureSherpaModel(ctx, ""); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(filepath.Join(d, "tokens.txt")); err != nil {
		return nil, &WakeWordError{Class: "RuntimeError", Msg: "sherpa KWS model not found at " + d}
	}
	phraseValue := wakeGet(cfg, "phrase")
	if !pyTruthy(phraseValue) {
		phraseValue = "hey hermes"
	}
	phrase := pystr.Strip(pyrepr.Str(phraseValue))
	var phrases []string
	phraseMap := map[string]string{}
	setdefault := func(p, profile string) {
		if _, ok := phraseMap[p]; !ok {
			phraseMap[p] = profile
			phrases = append(phrases, p)
		}
	}
	setdefault(phrase, wakeActiveProfileName(ctx))
	routing, present := cfg["profile_routing"]
	if !present || pyTruthy(routing) {
		for _, enrolled := range EnrolledProfilePhrases(ctx) {
			setdefault(pystr.Strip(enrolled.Phrase), enrolled.Profile)
		}
	}
	upper := make([]string, len(phrases))
	for i, p := range phrases {
		upper[i] = pystr.Upper(p)
	}
	tokens, err := sherpa.Text2Token(upper, filepath.Join(d, "tokens.txt"), "bpe", filepath.Join(d, "bpe.model"))
	if err != nil {
		return nil, err
	}
	e := &sherpaKwsEngine{wakeEngineBase: wakeEngineBase{frameLength: 1280}, displayToProfile: map[string]string{}}
	tmpDir, err := pypath.Gettempdir()
	if err != nil {
		return nil, err
	}
	kw, err := os.CreateTemp(tmpDir, "icebath-kws-*.txt")
	if err != nil {
		return nil, err
	}
	newline := "\n"
	if runtime.GOOS == "windows" {
		newline = "\r\n"
	}
	for i := 0; i < len(phrases) && i < len(tokens); i++ {
		display := strings.ReplaceAll(pystr.Upper(phrases[i]), " ", "_")
		e.displayToProfile[display] = phraseMap[phrases[i]]
		if _, err := kw.WriteString(strings.Join(tokens[i], " ") + " @" + display + newline); err != nil {
			kw.Close()
			return nil, err
		}
	}
	if err := kw.Close(); err != nil {
		return nil, err
	}
	e.keywordsFile = kw.Name()
	sensitivity, err := wakeSensitivity(cfg)
	if err != nil {
		return nil, err
	}
	modelFile := func(part string) (string, error) {
		hits := sherpaModelGlob(d, part)
		if len(hits) == 0 {
			return "", &WakeWordError{Class: "RuntimeError", Msg: fmt.Sprintf("sherpa KWS model file missing: %s/%s-*[!8].onnx", d, part)}
		}
		return hits[0], nil
	}
	spec := sherpaKWSConfig{Tokens: filepath.Join(d, "tokens.txt"), KeywordsFile: e.keywordsFile, KeywordsThreshold: 0.05 + 0.4*sensitivity, NumThreads: 1}
	for _, part := range []struct {
		name string
		dst  *string
	}{{"encoder", &spec.Encoder}, {"decoder", &spec.Decoder}, {"joiner", &spec.Joiner}} {
		if *part.dst, err = modelFile(part.name); err != nil {
			return nil, err
		}
	}
	if e.spotter, err = sherpa.KeywordSpotter(spec); err != nil {
		return nil, err
	}
	if e.stream, err = e.spotter.CreateStream(); err != nil {
		return nil, err
	}
	return e, nil
}

// Sorted pathlib glob of part-*[!8].onnx in dir, case folded on Windows like normcase
func sherpaModelGlob(dir, part string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	fold := func(s string) string { return s }
	if runtime.GOOS == "windows" {
		fold = strings.ToLower
	}
	prefix, suffix := fold(part+"-"), fold(".onnx")
	var hits []string
	for _, entry := range entries {
		name := fold(entry.Name())
		if len(name) < len(prefix)+1+len(suffix) || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		if name[len(name)-len(suffix)-1] == '8' {
			continue
		}
		hits = append(hits, filepath.Join(dir, entry.Name()))
	}
	slices.SortFunc(hits, func(a, b string) int { return strings.Compare(fold(a), fold(b)) })
	return hits
}

// Feeds one frame and drains every ready decode, resetting the stream after each keyword so one utterance fires once
func (e *sherpaKwsEngine) Process(frame []int16) (bool, error) {
	samples := make([]float32, len(frame))
	for i, v := range frame {
		samples[i] = float32(v) / 32768.0
	}
	if err := e.stream.AcceptWaveform(WakeSampleRate, samples); err != nil {
		return false, err
	}
	fired := false
	for e.spotter.IsReady(e.stream) {
		if err := e.spotter.DecodeStream(e.stream); err != nil {
			return fired, err
		}
		if display := e.spotter.GetResult(e.stream); display != "" {
			fired = true
			e.matchMu.Lock()
			e.lastMatch = &[2]string{pystr.Lower(strings.ReplaceAll(display, "_", " ")), e.displayToProfile[display]}
			e.matchMu.Unlock()
			if err := e.spotter.ResetStream(e.stream); err != nil {
				return fired, err
			}
		}
	}
	return fired, nil
}

// LastMatch returns the phrase and profile of the latest keyword fire
func (e *sherpaKwsEngine) LastMatch() (string, string, bool) {
	e.matchMu.Lock()
	defer e.matchMu.Unlock()
	if e.lastMatch == nil {
		return "", "", false
	}
	return e.lastMatch[0], e.lastMatch[1], true
}

// Replaces the stream so buffered audio and decoder state from before a pause cannot fire
func (e *sherpaKwsEngine) Reset() error {
	if s, err := e.spotter.CreateStream(); err == nil {
		e.stream = s
	}
	return nil
}

func (e *sherpaKwsEngine) Close() {
	_ = os.Remove(e.keywordsFile)
}

// The pvporcupine names the engine imports
type pvporcupineModule interface {
	Create(accessKey string, keywordPaths, keywords []string, sensitivities []float64) (pvPorcupine, error)
}

// One Porcupine handle
type pvPorcupine interface {
	FrameLength() int
	Process(pcm []int16) (int, error)
	Delete()
}

// Picovoice Porcupine engine, its looser sensitivity inverted so higher stays stricter
type porcupineEngine struct {
	wakeEngineBase
	porcupine pvPorcupine
}

// Builds the Porcupine engine from PORCUPINE_ACCESS_KEY and a built in keyword or keyword file
func newPorcupineEngine(ctx context.Context, cfg map[string]any) (WakeEngine, error) {
	if err := wakeEnsureDep(ctx, "wake-porcupine", cfg); err != nil {
		return nil, err
	}
	sub := wakeSub(cfg, "porcupine")
	pv, err := wakeImportPvporcupine(ctx)
	if err != nil {
		return nil, err
	}
	accessKey := pystr.Strip(os.Getenv("PORCUPINE_ACCESS_KEY"))
	if accessKey == "" {
		return nil, &WakeWordError{Class: "RuntimeError", Msg: "Porcupine wake word requires PORCUPINE_ACCESS_KEY " +
			"(get a free key at https://console.picovoice.ai)."}
	}
	keywordValue := any("jarvis")
	if pyTruthy(sub["keyword"]) {
		keywordValue = sub["keyword"]
	}
	keyword := pystr.Strip(pyrepr.Str(keywordValue))
	sensitivity, err := wakeSensitivity(cfg)
	if err != nil {
		return nil, err
	}
	var keywordPaths, keywords []string
	if looksLikePath(keyword) {
		keywordPaths = []string{keyword}
	} else {
		keywords = []string{keyword}
	}
	handle, err := pv.Create(accessKey, keywordPaths, keywords, []float64{1.0 - sensitivity})
	if err != nil {
		return nil, err
	}
	return &porcupineEngine{wakeEngineBase: wakeEngineBase{frameLength: handle.FrameLength()}, porcupine: handle}, nil
}

func (e *porcupineEngine) Process(frame []int16) (bool, error) {
	index, err := e.porcupine.Process(frame)
	return index >= 0, err
}

func (e *porcupineEngine) Close() { e.porcupine.Delete() }
