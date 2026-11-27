// The open vocabulary wake engine: a keyword spotter of the native speech engine listening for the wake phrase of every enrolled profile
package service

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	piperconfig "github.com/nickheyer/icebath/internal/plugins/defaults/speech/piper/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/sherpa/config"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/speech/sentencepiece"
	"github.com/nickheyer/icebath/internal/speech/sherpa"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Keyword spotting model of the speech engine project, pinned
const modelDir = "sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01"

var model = speech.Asset{Name: modelDir + ".tar.bz2", Size: 17626723,
	URL:    "https://github.com/k2-fsa/sherpa-onnx/releases/download/kws-models/" + modelDir + ".tar.bz2",
	SHA256: "f170013b4716e41b62b9bfd809687c207cef798ef9bc6534d524e17af9b6561a"}

// Samples of one frame the engine scores, 80 ms at 16 kHz
const frame = 1280

func Register(registry *speech.Registry) {
	registry.Register(speech.Services{Waking: []speech.Engine{{
		Name: config.Name, Aliases: []string{"sherpa-onnx", "kws", "open"}, Title: "Sherpa",
		About: "Hears any wake phrase, those of every profile among them, on the native speech engine", Rank: 2, Open: true,
		Supported: func(*speech.Speech) bool { return sherpa.Supported() },
		Installed: func(*speech.Speech) bool { return sherpa.Supported() },
		New:       open,
	}}})
}

// Folder of the model: the one set, else one already in the profile's wake word cache, else the bundled one, fetched once
func folder(ctx context.Context, wake *speech.Wake) (string, error) {
	if own := wake.Setting(config.ModelDir); own != "" {
		if !fsx.IsFile(filepath.Join(own, "tokens.txt")) {
			return "", fmt.Errorf("sherpa KWS model not found at %s", own)
		}
		return own, nil
	}
	if cfg, err := wake.Speech.Profile(wake.Profile); err == nil {
		if cached := filepath.Join(cfg.ProfileDir(cfg.Profile()), "cache", "wakewords", modelDir); fsx.IsFile(filepath.Join(cached, "tokens.txt")) {
			return cached, nil
		}
	}
	if !wake.Speech.Fetched(model) {
		slog.Info("wake word: downloading the sherpa KWS model (about 17 MB, once)")
	}
	tree, err := wake.Speech.Tree(ctx, model)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(tree, modelDir)
	if !fsx.IsFile(filepath.Join(dir, "tokens.txt")) {
		return "", fmt.Errorf("sherpa KWS model archive lacks tokens.txt: %s", dir)
	}
	return dir, nil
}

// File of a part of the model: the first part-*.onnx that is not the int8 one, in name order, folded on Windows
func part(dir, name string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	fold := func(s string) string { return s }
	if runtime.GOOS == "windows" {
		fold = strings.ToLower
	}
	prefix, suffix := fold(name+"-"), fold(".onnx")
	var hits []string
	for _, entry := range entries {
		n := fold(entry.Name())
		if len(n) > len(prefix)+len(suffix) && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) && n[len(n)-len(suffix)-1] != '8' {
			hits = append(hits, filepath.Join(dir, entry.Name()))
		}
	}
	if len(hits) == 0 {
		return "", fmt.Errorf("sherpa KWS model file missing: %s/%s-*[!8].onnx", dir, name)
	}
	slices.SortFunc(hits, func(a, b string) int { return strings.Compare(fold(a), fold(b)) })
	return hits[0], nil
}

// Token table of the model by symbol
func table(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]bool{}
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		if fields := strings.Fields(lines.Text()); len(fields) == 2 {
			out[fields[0]] = true
		}
	}
	return out, lines.Err()
}

// Builds the spotter: each phrase spelled in the model's pieces into a keywords file under its display name, the threshold from the
// sensitivity
func open(ctx context.Context, wake *speech.Wake) (speech.Hotword, error) {
	// The engine of this process on the device the profile's Piper setting asks for, as every user of the engine follows it; the build this
	// process already runs when Piper loaded the other one here
	gpu := false
	if cfg, err := wake.Speech.Profile(wake.Profile); err == nil {
		gpu = cfg.GetBool(piperconfig.CUDA)
	}
	module, err := sherpa.Load(ctx, wake.Speech, gpu)
	if err != nil && strings.Contains(err.Error(), "already runs on the") {
		module, err = sherpa.Load(ctx, wake.Speech, !gpu)
	}
	if err != nil {
		return nil, err
	}
	dir, err := folder(ctx, wake)
	if err != nil {
		return nil, err
	}
	var phrases []string
	owners := map[string]string{}
	add := func(phrase, profile string) {
		if phrase = strings.TrimSpace(phrase); phrase != "" {
			if _, ok := owners[phrase]; !ok {
				owners[phrase] = profile
				phrases = append(phrases, phrase)
			}
		}
	}
	add(wake.Phrase, wake.Profile)
	if wake.Routing {
		for _, enrolled := range wake.Profiles {
			add(enrolled.Phrase, enrolled.Profile)
		}
	}
	pieces, err := sentencepiece.Load(filepath.Join(dir, "bpe.model"))
	if err != nil {
		return nil, err
	}
	known, err := table(filepath.Join(dir, "tokens.txt"))
	if err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp("", "icebath-kws-*.txt")
	if err != nil {
		return nil, err
	}
	newline := "\n"
	if runtime.GOOS == "windows" {
		newline = "\r\n"
	}
	e := &engine{owners: map[string]string{}, keywords: temp.Name()}
	for _, phrase := range phrases {
		spelled, err := pieces.Pieces(strings.ToUpper(phrase))
		if err != nil {
			temp.Close()
			return nil, err
		}
		if missing := slices.IndexFunc(spelled, func(p string) bool { return !known[p] }); missing >= 0 {
			slog.Warn(fmt.Sprintf("Can't find token %s in token table, check your tokens.txt see if %s in it. skipping text : %s.", spelled[missing],
				spelled[missing], strings.ToUpper(phrase)))
			continue
		}
		display := strings.ReplaceAll(strings.ToUpper(phrase), " ", "_")
		e.owners[display] = owners[phrase]
		if _, err := temp.WriteString(strings.Join(spelled, " ") + " @" + display + newline); err != nil {
			temp.Close()
			return nil, err
		}
	}
	if err := temp.Close(); err != nil {
		return nil, err
	}
	options := sherpa.SpotterOptions{Tokens: filepath.Join(dir, "tokens.txt"), KeywordsFile: e.keywords, Threshold: 0.05 + 0.4*wake.Sensitivity, Threads: 1}
	for name, into := range map[string]*string{"encoder": &options.Encoder, "decoder": &options.Decoder, "joiner": &options.Joiner} {
		if *into, err = part(dir, name); err != nil {
			os.Remove(e.keywords)
			return nil, err
		}
	}
	if e.spotter, err = module.NewSpotter(options); err != nil {
		os.Remove(e.keywords)
		return nil, err
	}
	e.stream = e.spotter.Stream()
	return e, nil
}

// Spotter over one stream, which knows the profile of each phrase it listens for
type engine struct {
	spotter  *sherpa.Spotter
	stream   *sherpa.Stream
	owners   map[string]string
	keywords string
	mu       sync.Mutex
	matched  bool
	phrase   string
	profile  string
}

func (e *engine) FrameLength() int { return frame }

// Feeds one frame and drains every ready decode, the stream reset after each keyword so one utterance fires once
func (e *engine) Process(samples []int16) (bool, error) {
	floats := make([]float32, len(samples))
	for i, v := range samples {
		floats[i] = float32(v) / 32768
	}
	e.stream.Accept(speech.WakeRate, floats)
	fired := false
	for e.spotter.Ready(e.stream) {
		e.spotter.Decode(e.stream)
		if display := e.spotter.Keyword(e.stream); display != "" {
			fired = true
			e.mu.Lock()
			e.matched, e.phrase, e.profile = true, strings.ToLower(strings.ReplaceAll(display, "_", " ")), e.owners[display]
			e.mu.Unlock()
			e.spotter.Reset(e.stream)
		}
	}
	return fired, nil
}

func (e *engine) LastMatch() (string, string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.phrase, e.profile, e.matched
}

// A new stream, so audio and decoder state from before a pause cannot fire
func (e *engine) Reset() error {
	e.stream.Close()
	e.stream = e.spotter.Stream()
	return nil
}

func (e *engine) Close() {
	e.stream.Close()
	e.spotter.Close()
	os.Remove(e.keywords)
}
