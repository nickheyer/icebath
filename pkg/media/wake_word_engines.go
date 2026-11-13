package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/fetch"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/openwakeword"
	"github.com/nickheyer/icebath/pkg/internal/porcupine"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/sherpaonnx"
)

// WakeEngine is the hotword engine contract, int16 frames in and a fire verdict out
type WakeEngine interface {
	FrameLength() int
	Process(frame []int16) (bool, error)
	Reset() error
	Close()
}

// Engine sub section of the wake config, empty unless a mapping
func wakeSub(cfg map[string]any, key string) map[string]any {
	if sub, ok := asStrMap(cfg[key]); ok {
		return sub
	}
	return map[string]any{}
}

// Stripped string of a config value, empty for unset or falsy values
func wakeString(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Configured openWakeWord model reference, empty when none is set
func openWakeWordModel(cfg map[string]any) string {
	return wakeString(wakeSub(cfg, "openwakeword")["model"])
}

// openWakeWord engine, sensitivity as the raw threshold with a confirmation streak
type openWakeWordEngine struct {
	threshold     float64
	confirmNeeded int
	confirmStreak int
	features      *openwakeword.Features
	model         *openwakeword.Model
}

// Builds the openWakeWord engine on the configured shipped model name or model file
func newOpenWakeWordEngine(ctx context.Context, cfg map[string]any) (WakeEngine, error) {
	lib, err := openwakeword.Load(ctx)
	if err != nil {
		return nil, err
	}
	ref := openWakeWordModel(cfg)
	if ref == "" {
		return nil, &WakeWordError{Msg: "wake_word.openwakeword.model is not set, name a .tflite model file or one of " +
			strings.Join(lib.Builtin(), ", ") + ", or use wake_word.provider: sherpa to listen for the phrase itself"}
	}
	path, err := lib.ModelPath(ref)
	if err != nil {
		return nil, err
	}
	e := &openWakeWordEngine{}
	if e.threshold, err = wakeSensitivity(cfg); err != nil {
		return nil, err
	}
	if e.confirmNeeded, err = wakeConfirmationFrames(cfg); err != nil {
		return nil, err
	}
	if e.features, err = lib.NewFeatures(); err != nil {
		return nil, err
	}
	if e.model, err = lib.NewModel(path); err != nil {
		e.features.Close()
		return nil, err
	}
	return e, nil
}

func (e *openWakeWordEngine) FrameLength() int { return openwakeword.SamplesPerChunk }

// Scores one frame and fires after the configured run of consecutive over threshold frames
func (e *openWakeWordEngine) Process(frame []int16) (bool, error) {
	chunk := make([]byte, 2*len(frame))
	for i, v := range frame {
		binary.NativeEndian.PutUint16(chunk[2*i:], uint16(v))
	}
	over := false
	err := e.features.Process(chunk, func(emb []float32) error {
		return e.model.Process(emb, func(score float32) {
			if float64(score) >= e.threshold {
				over = true
			}
		})
	})
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
	e.features.Close()
	e.model.Close()
}

const sherpaKWSModelDir = "sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01"

// Keyword spotting model archive of the speech engine project, pinned by size and sha256
var sherpaKWSModel = fetch.Asset{Name: sherpaKWSModelDir + ".tar.bz2", Size: 17626723,
	URL:    "https://github.com/k2-fsa/sherpa-onnx/releases/download/kws-models/" + sherpaKWSModelDir + ".tar.bz2",
	SHA256: "f170013b4716e41b62b9bfd809687c207cef798ef9bc6534d524e17af9b6561a"}

// Directory of the sherpa KWS model, one already under the profile cache else the pinned archive fetched once
func ensureSherpaModel(ctx context.Context) (string, error) {
	if target := filepath.Join(home.Dir(ctx), "cache", "wakewords", sherpaKWSModelDir); fileExists(filepath.Join(target, "tokens.txt")) {
		return target, nil
	}
	wakeLog().Info("wake word: preparing the sherpa KWS model (first use downloads about 17 MB)")
	tree, err := fetch.Tree(ctx, sherpaKWSModel)
	if err != nil {
		return "", err
	}
	target := filepath.Join(tree, sherpaKWSModelDir)
	if !fileExists(filepath.Join(target, "tokens.txt")) {
		return "", &WakeWordError{Msg: "sherpa KWS model archive lacks tokens.txt: " + target}
	}
	return target, nil
}

// Open vocabulary sherpa keyword spotter listening for every enrolled profile phrase
type sherpaKwsEngine struct {
	displayToProfile map[string]string
	keywordsFile     string
	spotter          *sherpaonnx.KeywordSpotter
	stream           *sherpaonnx.OnlineStream
	matchMu          sync.Mutex
	lastMatch        *[2]string
}

// Wake phrases of the config with the profile each one routes to, the configured phrase first
func wakePhrases(ctx context.Context, cfg map[string]any) ([]string, map[string]string) {
	phrase := wakeString(wakeGet(cfg, "phrase"))
	if phrase == "" {
		phrase = wakeDefaultPhrase
	}
	var phrases []string
	routes := map[string]string{}
	add := func(p, profile string) {
		if _, ok := routes[p]; !ok && p != "" {
			routes[p] = profile
			phrases = append(phrases, p)
		}
	}
	add(phrase, wakeActiveProfileName(ctx))
	if routing, present := cfg["profile_routing"]; !present || pyTruthy(routing) {
		for _, enrolled := range EnrolledProfilePhrases(ctx) {
			add(pystr.Strip(enrolled.Phrase), enrolled.Profile)
		}
	}
	return phrases, routes
}

// Builds the sherpa engine, tokenizing every phrase against the model vocabulary into a keywords file
func newSherpaKwsEngine(ctx context.Context, cfg map[string]any) (WakeEngine, error) {
	module, err := wakeSpeechEngine(ctx)
	if err != nil {
		return nil, err
	}
	d := wakeString(wakeSub(cfg, "sherpa")["model_dir"])
	if d == "" {
		if d, err = ensureSherpaModel(ctx); err != nil {
			return nil, err
		}
	}
	tokensFile := filepath.Join(d, "tokens.txt")
	if _, err := os.Stat(tokensFile); err != nil {
		return nil, &WakeWordError{Msg: "sherpa KWS model not found at " + d}
	}
	options, err := sherpaonnx.KeywordTokenOptions(d)
	if err != nil {
		return nil, err
	}
	phrases, routes := wakePhrases(ctx, cfg)
	upper := make([]string, len(phrases))
	for i, p := range phrases {
		upper[i] = pystr.Upper(p)
	}
	tokens, err := sherpaonnx.Text2Token(ctx, upper, tokensFile, options)
	if err != nil {
		return nil, err
	}
	e := &sherpaKwsEngine{displayToProfile: map[string]string{}}
	kw, err := os.CreateTemp("", "icebath-kws-*.txt")
	if err != nil {
		return nil, err
	}
	newline := "\n"
	if runtime.GOOS == "windows" {
		newline = "\r\n"
	}
	written := 0
	for i, toks := range tokens {
		if len(toks) == 0 {
			continue
		}
		display := strings.ReplaceAll(pystr.Upper(phrases[i]), " ", "_")
		e.displayToProfile[display] = routes[phrases[i]]
		if _, err := kw.WriteString(strings.Join(toks, " ") + " @" + display + newline); err != nil {
			kw.Close()
			os.Remove(kw.Name())
			return nil, err
		}
		written++
	}
	if err := kw.Close(); err != nil {
		return nil, err
	}
	e.keywordsFile = kw.Name()
	if written == 0 {
		os.Remove(e.keywordsFile)
		return nil, &WakeWordError{Msg: fmt.Sprintf("none of the wake phrases %s can be spelled with the tokens of the keyword model at %s", strings.Join(phrases, ", "), d)}
	}
	sensitivity, err := wakeSensitivity(cfg)
	if err != nil {
		return nil, err
	}
	spec := sherpaonnx.KeywordSpotterOptions{Tokens: tokensFile, KeywordsFile: e.keywordsFile, KeywordsThreshold: 0.05 + 0.4*sensitivity, NumThreads: 1}
	for _, part := range []struct {
		name string
		dst  *string
	}{{"encoder", &spec.Encoder}, {"decoder", &spec.Decoder}, {"joiner", &spec.Joiner}} {
		hits := sherpaModelGlob(d, part.name)
		if len(hits) == 0 {
			os.Remove(e.keywordsFile)
			return nil, &WakeWordError{Msg: fmt.Sprintf("sherpa KWS model file missing: %s/%s-*[!8].onnx", d, part.name)}
		}
		*part.dst = hits[0]
	}
	if e.spotter, err = module.NewKeywordSpotter(spec); err != nil {
		os.Remove(e.keywordsFile)
		return nil, err
	}
	e.stream = e.spotter.CreateStream()
	return e, nil
}

// Sorted part-*[!8].onnx files in dir, case folded on Windows
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

func (e *sherpaKwsEngine) FrameLength() int { return 1280 }

// Feeds one frame and drains every ready decode, resetting the stream after each keyword so one utterance fires once
func (e *sherpaKwsEngine) Process(frame []int16) (bool, error) {
	samples := make([]float32, len(frame))
	for i, v := range frame {
		samples[i] = float32(v) / 32768.0
	}
	e.stream.AcceptWaveform(WakeSampleRate, samples)
	fired := false
	for e.spotter.IsReady(e.stream) {
		e.spotter.DecodeStream(e.stream)
		if display := e.spotter.GetResult(e.stream); display != "" {
			fired = true
			e.matchMu.Lock()
			e.lastMatch = &[2]string{pystr.Lower(strings.ReplaceAll(display, "_", " ")), e.displayToProfile[display]}
			e.matchMu.Unlock()
			e.spotter.ResetStream(e.stream)
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
	e.stream = e.spotter.CreateStream()
	return nil
}

func (e *sherpaKwsEngine) Close() {
	_ = os.Remove(e.keywordsFile)
}

// Picovoice Porcupine engine, its looser sensitivity inverted so higher stays stricter
type porcupineEngine struct {
	detector *porcupine.Engine
}

// Builds the Porcupine engine from PORCUPINE_ACCESS_KEY and a built in keyword or keyword file
func newPorcupineEngine(ctx context.Context, cfg map[string]any) (WakeEngine, error) {
	accessKey := pystr.Strip(os.Getenv("PORCUPINE_ACCESS_KEY"))
	if accessKey == "" {
		return nil, &WakeWordError{Msg: "Porcupine wake word requires PORCUPINE_ACCESS_KEY (get a free key at https://console.picovoice.ai)."}
	}
	lib, err := porcupine.Load(ctx)
	if err != nil {
		return nil, err
	}
	keyword := wakeString(wakeSub(cfg, "porcupine")["keyword"])
	if keyword == "" {
		keyword = "jarvis"
	}
	sensitivity, err := wakeSensitivity(cfg)
	if err != nil {
		return nil, err
	}
	detector, err := lib.New(accessKey, []string{keyword}, []float64{1.0 - sensitivity})
	if err != nil {
		return nil, err
	}
	return &porcupineEngine{detector: detector}, nil
}

func (e *porcupineEngine) FrameLength() int { return e.detector.FrameLength }

func (e *porcupineEngine) Process(frame []int16) (bool, error) {
	index, err := e.detector.Process(frame)
	return index >= 0, err
}

func (e *porcupineEngine) Reset() error { return nil }

func (e *porcupineEngine) Close() { e.detector.Close() }
